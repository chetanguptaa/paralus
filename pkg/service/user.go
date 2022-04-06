package service

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	bun "github.com/uptrace/bun"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/RafayLabs/rcloud-base/internal/dao"
	"github.com/RafayLabs/rcloud-base/internal/models"
	providers "github.com/RafayLabs/rcloud-base/internal/provider/kratos"
	"github.com/RafayLabs/rcloud-base/pkg/common"
	"github.com/RafayLabs/rcloud-base/pkg/query"
	userrpcv3 "github.com/RafayLabs/rcloud-base/proto/rpc/user"
	authzv1 "github.com/RafayLabs/rcloud-base/proto/types/authz"
	commonv3 "github.com/RafayLabs/rcloud-base/proto/types/commonpb/v3"
	v3 "github.com/RafayLabs/rcloud-base/proto/types/commonpb/v3"
	userv3 "github.com/RafayLabs/rcloud-base/proto/types/userpb/v3"
)

const (
	userKind     = "User"
	userListKind = "UserList"
)

// GroupService is the interface for group operations
type UserService interface {
	// create user
	Create(context.Context, *userv3.User) (*userv3.User, error)
	// get user by id
	GetByID(context.Context, *userv3.User) (*userv3.User, error)
	// get user by name
	GetByName(context.Context, *userv3.User) (*userv3.User, error)
	// get full user info
	GetUserInfo(context.Context, *userv3.User) (*userv3.UserInfo, error)
	// create or update user
	Update(context.Context, *userv3.User) (*userv3.User, error)
	// delete user
	Delete(context.Context, *userv3.User) (*userrpcv3.DeleteUserResponse, error)
	// list users
	List(context.Context, ...query.Option) (*userv3.UserList, error)
	// retrieve the cli config for the logged in user
	RetrieveCliConfig(ctx context.Context, req *userrpcv3.ApiKeyRequest) (*common.CliConfigDownloadData, error)
}

type userService struct {
	ap  providers.AuthProvider
	db  *bun.DB
	azc AuthzService
	ks  ApiKeyService
	cc  common.CliConfigDownloadData
}

type userTraits struct {
	Email       string
	FirstName   string
	LastName    string
	Description string
}

// FIXME: find a better way to do this
type parsedIds struct {
	Id           uuid.UUID
	Partner      uuid.UUID
	Organization uuid.UUID
}

func NewUserService(ap providers.AuthProvider, db *bun.DB, azc AuthzService, kss ApiKeyService, cfg common.CliConfigDownloadData) UserService {
	return &userService{ap: ap, db: db, azc: azc, ks: kss, cc: cfg}
}

func getUserTraits(traits map[string]interface{}) userTraits {
	// FIXME: is there a better way to do this?
	// All of these should ideally be available as we have the identities schema, but just in case
	email, ok := traits["email"]
	if !ok {
		email = ""
	}
	fname, ok := traits["first_name"]
	if !ok {
		fname = ""
	}
	lname, ok := traits["last_name"]
	if !ok {
		lname = ""
	}
	desc, ok := traits["desc"]
	if !ok {
		desc = ""
	}
	return userTraits{
		Email:       email.(string),
		FirstName:   fname.(string),
		LastName:    lname.(string),
		Description: desc.(string),
	}
}

// Map roles to accounts
func (s *userService) createUserRoleRelations(ctx context.Context, db bun.IDB, user *userv3.User, ids parsedIds) (*userv3.User, []uuid.UUID, error) {
	projectNamespaceRoles := user.GetSpec().GetProjectNamespaceRoles()

	var pars []models.ProjectAccountResourcerole
	var ars []models.AccountResourcerole
	var ps []*authzv1.Policy
	var rids []uuid.UUID
	for _, pnr := range projectNamespaceRoles {
		role := pnr.GetRole()
		entity, err := dao.GetByName(ctx, db, role, &models.Role{})
		if err != nil {
			return &userv3.User{}, nil, fmt.Errorf("unable to find role '%v'", role)
		}
		var roleId uuid.UUID
		var roleName string
		var scope string
		if rle, ok := entity.(*models.Role); ok {
			roleId = rle.ID
			roleName = rle.Name
			rids = append(rids, rle.ID)
			scope = strings.ToLower(rle.Scope)
		} else {
			return &userv3.User{}, nil, fmt.Errorf("unable to find role '%v'", role)
		}

		project := pnr.GetProject()
		org := user.GetMetadata().GetOrganization()

		switch scope {
		case "system":
			ar := models.AccountResourcerole{
				CreatedAt:      time.Now(),
				ModifiedAt:     time.Now(),
				Trash:          false,
				Default:        true,
				RoleId:         roleId,
				PartnerId:      ids.Partner,
				OrganizationId: ids.Organization, // Not really used
				AccountId:      ids.Id,
				Active:         true,
			}
			ars = append(ars, ar)

			ps = append(ps, &authzv1.Policy{
				Sub:  "u:" + user.GetMetadata().GetName(),
				Ns:   "*",
				Proj: "*",
				Org:  "*",
				Obj:  role,
			})
		case "organization":
			if org == "" {
				return &userv3.User{}, nil, fmt.Errorf("no org name provided for role '%v'", roleName)
			}

			ar := models.AccountResourcerole{
				CreatedAt:      time.Now(),
				ModifiedAt:     time.Now(),
				Trash:          false,
				Default:        true,
				RoleId:         roleId,
				PartnerId:      ids.Partner,
				OrganizationId: ids.Organization,
				AccountId:      ids.Id,
				Active:         true,
			}
			ars = append(ars, ar)

			ps = append(ps, &authzv1.Policy{
				Sub:  "u:" + user.GetMetadata().GetName(),
				Ns:   "*",
				Proj: "*",
				Org:  org,
				Obj:  role,
			})
		case "project":
			if org == "" {
				return &userv3.User{}, nil, fmt.Errorf("no org name provided for role '%v'", roleName)
			}
			if project == "" {
				return &userv3.User{}, nil, fmt.Errorf("no project name provided for role '%v'", roleName)
			}
			projectId, err := dao.GetProjectId(ctx, db, project)
			if err != nil {
				return user, nil, fmt.Errorf("unable to find project '%v'", project)
			}
			par := models.ProjectAccountResourcerole{
				CreatedAt:      time.Now(),
				ModifiedAt:     time.Now(),
				Trash:          false,
				Default:        true,
				RoleId:         roleId,
				PartnerId:      ids.Partner,
				OrganizationId: ids.Organization,
				AccountId:      ids.Id,
				ProjectId:      projectId,
				Active:         true,
			}
			pars = append(pars, par)

			ps = append(ps, &authzv1.Policy{
				Sub:  "u:" + user.GetMetadata().GetName(),
				Ns:   "*",
				Proj: project,
				Org:  org,
				Obj:  role,
			})
		default:
			if err != nil {
				return user, nil, fmt.Errorf("namespace specific roles are not handled")
			}
		}
	}
	if len(pars) > 0 {
		_, err := dao.Create(ctx, db, &pars)
		if err != nil {
			return &userv3.User{}, nil, err
		}
	}
	if len(ars) > 0 {
		_, err := dao.Create(ctx, db, &ars)
		if err != nil {
			return &userv3.User{}, nil, err
		}
	}

	if len(ps) > 0 {
		success, err := s.azc.CreatePolicies(ctx, &authzv1.Policies{Policies: ps})
		if err != nil || !success.Res {
			return &userv3.User{}, nil, fmt.Errorf("unable to create mapping in authz; %v", err)
		}
	}

	return user, rids, nil
}

// Update the groups mapped to each user(account)
func (s *userService) createGroupAccountRelations(ctx context.Context, db bun.IDB, userId uuid.UUID, usr *userv3.User) (*userv3.User, []uuid.UUID, error) {
	var grpaccs []models.GroupAccount
	var ugs []*authzv1.UserGroup
	var ids []uuid.UUID
	for _, group := range unique(usr.GetSpec().GetGroups()) {
		// FIXME: do combined lookup
		entity, err := dao.GetByName(ctx, s.db, group, &models.Group{})
		if err != nil {
			return &userv3.User{}, nil, fmt.Errorf("unable to find group '%v'", group)
		}
		if grp, ok := entity.(*models.Group); ok {
			grp := models.GroupAccount{
				CreatedAt:  time.Now(),
				ModifiedAt: time.Now(),
				Trash:      false,
				AccountId:  userId,
				GroupId:    grp.ID,
				Active:     true,
			}
			ids = append(ids, grp.ID)
			grpaccs = append(grpaccs, grp)
			ugs = append(ugs, &authzv1.UserGroup{
				Grp:  "g:" + group,
				User: "u:" + usr.Metadata.Name,
			})
		}
	}
	if len(grpaccs) == 0 {
		return usr, []uuid.UUID{}, nil
	}
	_, err := dao.Create(ctx, db, &grpaccs)
	if err != nil {
		return &userv3.User{}, []uuid.UUID{}, err
	}

	_, err = s.azc.CreateUserGroups(ctx, &authzv1.UserGroups{UserGroups: ugs})
	if err != nil {
		return &userv3.User{}, []uuid.UUID{}, fmt.Errorf("unable to create mapping in authz; %v", err)
	}

	return usr, ids, nil
}

func (s *userService) deleteGroupAccountRelations(ctx context.Context, db bun.IDB, userId uuid.UUID, usr *userv3.User) (*userv3.User, []uuid.UUID, error) {
	ugs := []models.GroupAccount{}
	ids := []uuid.UUID{}
	err := dao.DeleteXR(ctx, db, "account_id", userId, &ugs)
	if err != nil {
		return &userv3.User{}, ids, fmt.Errorf("unable to delete user; %v", err)
	}

	_, err = s.azc.DeleteUserGroups(ctx, &authzv1.UserGroup{Grp: "u:" + usr.GetMetadata().GetName()})
	if err != nil {
		return &userv3.User{}, ids, fmt.Errorf("unable to delete group-user relations from authz; %v", err)
	}

	for _, ug := range ugs {
		ids = append(ids, ug.GroupId)
	}
	return usr, ids, nil
}

// FIXME: make this generic
func (s *userService) getPartnerOrganization(ctx context.Context, db bun.IDB, user *userv3.User) (uuid.UUID, uuid.UUID, error) {
	partner := user.GetMetadata().GetPartner()
	org := user.GetMetadata().GetOrganization()
	partnerId, err := dao.GetPartnerId(ctx, db, partner)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	organizationId, err := dao.GetOrganizationId(ctx, db, org)
	if err != nil {
		return partnerId, uuid.Nil, err
	}
	return partnerId, organizationId, nil

}

func (s *userService) Create(ctx context.Context, user *userv3.User) (*userv3.User, error) {
	partnerId, organizationId, err := s.getPartnerOrganization(ctx, s.db, user)
	if err != nil {
		return nil, fmt.Errorf("unable to get partner and org id")
	}

	// Kratos checks if the user is already available
	id, err := s.ap.Create(ctx, map[string]interface{}{
		"email":       user.GetMetadata().GetName(), // can be just username for API access
		"first_name":  user.GetSpec().GetFirstName(),
		"last_name":   user.GetSpec().GetLastName(),
		"description": user.GetMetadata().GetDescription(),
	})
	if err != nil {
		return &userv3.User{}, err
	}

	uid, _ := uuid.Parse(id)

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return &userv3.User{}, err
	}

	user, rolesAfter, err := s.createUserRoleRelations(ctx, tx, user, parsedIds{Id: uid, Partner: partnerId, Organization: organizationId})
	if err != nil {
		tx.Rollback()
		return &userv3.User{}, err
	}

	user, groupsAfter, err := s.createGroupAccountRelations(ctx, tx, uuid.MustParse(id), user)
	if err != nil {
		tx.Rollback()
		return &userv3.User{}, err
	}

	err = tx.Commit()
	if err != nil {
		tx.Rollback()
		_log.Warn("unable to commit changes", err)
	}

	rl, err := s.ap.GetRecoveryLink(ctx, id)
	if err != nil {
		_log.Warn("unable to generate recovery url", err)
		return &userv3.User{}, err
	}
	user.Spec.RecoveryUrl = &rl

	CreateUserAuditEvent(ctx, s.db, AuditActionCreate, user.GetMetadata().GetName(), uid, []uuid.UUID{}, rolesAfter, []uuid.UUID{}, groupsAfter)
	return user, nil
}

func (s *userService) identitiesModelToUser(ctx context.Context, db bun.IDB, user *userv3.User, usr *models.KratosIdentities) (*userv3.User, error) {
	traits := getUserTraits(usr.Traits)
	groups, err := dao.GetGroups(ctx, db, usr.ID)
	if err != nil {
		return &userv3.User{}, err
	}
	groupNames := []string{}
	allAssociatedRoles := []*userv3.ProjectNamespaceRole{}
	for _, g := range groups {
		groupNames = append(groupNames, g.Name)

		//group roles
		groupRoles, err := dao.GetGroupRoles(ctx, db, g.ID)
		if err != nil {
			return &userv3.User{}, err
		}
		allAssociatedRoles = append(allAssociatedRoles, groupRoles...)
	}

	labels := make(map[string]string)

	roles, err := dao.GetUserRoles(ctx, db, usr.ID)
	if err != nil {
		return &userv3.User{}, err
	}
	roles = append(roles, allAssociatedRoles...)

	user.ApiVersion = apiVersion
	user.Kind = userKind
	user.Metadata = &v3.Metadata{
		Name:        traits.Email,
		Description: traits.Description,
		Labels:      labels,
		ModifiedAt:  timestamppb.New(usr.UpdatedAt),
		Id:          usr.ID.String(),
	}
	user.Spec = &userv3.UserSpec{
		FirstName:             traits.FirstName,
		LastName:              traits.LastName,
		Groups:                groupNames,
		ProjectNamespaceRoles: roles,
	}

	return user, nil
}

func (s *userService) GetByID(ctx context.Context, user *userv3.User) (*userv3.User, error) {
	id := user.GetMetadata().GetId()
	uid, err := uuid.Parse(id)
	if err != nil {
		return &userv3.User{}, err
	}
	entity, err := dao.GetByID(ctx, s.db, uid, &models.KratosIdentities{})
	if err != nil {
		return &userv3.User{}, err
	}

	if usr, ok := entity.(*models.KratosIdentities); ok {
		user, err := s.identitiesModelToUser(ctx, s.db, user, usr)
		if err != nil {
			return &userv3.User{}, err
		}

		return user, nil
	}
	return user, fmt.Errorf("unabele to fetch user '%v'", id)

}

func (s *userService) GetByName(ctx context.Context, user *userv3.User) (*userv3.User, error) {
	name := user.GetMetadata().GetName()
	entity, err := dao.GetByTraits(ctx, s.db, name, &models.KratosIdentities{})
	if err != nil {
		return &userv3.User{}, err
	}

	if usr, ok := entity.(*models.KratosIdentities); ok {
		user, err := s.identitiesModelToUser(ctx, s.db, user, usr)
		if err != nil {
			return &userv3.User{}, err
		}

		return user, nil
	}
	return user, nil
}

func (s *userService) GetUserInfo(ctx context.Context, user *userv3.User) (*userv3.UserInfo, error) {
	sd, ok := GetSessionDataFromContext(ctx)
	username := ""
	if !ok {
		return &userv3.UserInfo{}, fmt.Errorf("cannot get user info without auth")
	}
	username = sd.Username

	entity, err := dao.GetByTraits(ctx, s.db, username, &models.KratosIdentities{})
	if err != nil {
		return &userv3.UserInfo{}, err
	}

	roleMap := map[string][]string{}
	if usr, ok := entity.(*models.KratosIdentities); ok {
		user, err := s.identitiesModelToUser(ctx, s.db, user, usr)
		if err != nil {
			return &userv3.UserInfo{}, err
		}

		userinfo := &userv3.UserInfo{Metadata: user.Metadata}
		userinfo.ApiVersion = apiVersion
		userinfo.Kind = userKind
		userinfo.Spec = &userv3.UserInfoSpec{
			FirstName: user.Spec.FirstName,
			LastName:  user.Spec.LastName,
			Groups:    user.Spec.Groups,
		}
		permissions := []*userv3.Permission{}
		for _, p := range user.Spec.ProjectNamespaceRoles {
			rps, ok := roleMap[p.Role]
			if !ok {
				role, err := dao.GetIdByName(ctx, s.db, p.Role, &models.Role{})
				if err != nil {
					return &userv3.UserInfo{}, err
				}
				rle, ok := role.(*models.Role)
				if !ok {
					_log.Warn("unable to lookup existing role '%v'", p.Role)
					return &userv3.UserInfo{}, err
				}
				rpms, err := dao.GetRolePermissions(ctx, s.db, rle.ID)
				if err != nil {
					return &userv3.UserInfo{}, err
				}
				for _, r := range rpms {
					rps = append(rps, r.Name)
				}
				roleMap[p.Role] = rps
			}
			permissions = append(
				permissions,
				&userv3.Permission{
					Project:     p.Project,
					Namespace:   p.Namespace,
					Role:        p.Role,
					Permissions: rps,
				},
			)
		}
		userinfo.Spec.Permissions = permissions
		return userinfo, nil
	}
	return &userv3.UserInfo{}, fmt.Errorf("unable to get user info")
}

func (s *userService) deleteUserRoleRelations(ctx context.Context, db bun.IDB, userId uuid.UUID, user *userv3.User) ([]uuid.UUID, error) {
	ids := []uuid.UUID{}

	ar := []models.AccountResourcerole{}
	err := dao.DeleteXR(ctx, db, "account_id", userId, &ar)
	if err != nil {
		return nil, err
	}
	for _, r := range ar {
		ids = append(ids, r.RoleId)
	}

	par := []models.ProjectAccountResourcerole{}
	err = dao.DeleteXR(ctx, db, "account_id", userId, &par)
	if err != nil {
		return nil, err
	}
	for _, r := range par {
		ids = append(ids, r.RoleId)
	}

	panr := []models.ProjectAccountNamespaceRole{}
	err = dao.DeleteXR(ctx, db, "account_id", userId, &panr)
	if err != nil {
		return nil, err
	}
	for _, r := range panr {
		ids = append(ids, r.RoleId)
	}

	_, err = s.azc.DeletePolicies(ctx, &authzv1.Policy{Sub: "u:" + user.GetMetadata().GetName()})
	if err != nil {
		return nil, fmt.Errorf("unable to delete user-role relations from authz; %v", err)
	}

	return ids, nil
}

func (s *userService) Update(ctx context.Context, user *userv3.User) (*userv3.User, error) {
	name := user.GetMetadata().GetName()
	entity, err := dao.GetIdByTraits(ctx, s.db, name, &models.KratosIdentities{})
	if err != nil {
		return &userv3.User{}, fmt.Errorf("no user found with name '%v'", name)
	}

	if usr, ok := entity.(*models.KratosIdentities); ok {
		partnerId, organizationId, err := s.getPartnerOrganization(ctx, s.db, user)
		if err != nil {
			return nil, fmt.Errorf("unable to get partner and org id")
		}
		err = s.ap.Update(ctx, usr.ID.String(), map[string]interface{}{
			"email":       user.GetMetadata().GetName(),
			"first_name":  user.GetSpec().GetFirstName(),
			"last_name":   user.GetSpec().GetLastName(),
			"description": user.GetMetadata().GetDescription(),
		})
		if err != nil {
			return &userv3.User{}, err
		}

		tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
		if err != nil {
			return &userv3.User{}, err
		}

		rolesBefore, err := s.deleteUserRoleRelations(ctx, tx, usr.ID, user)
		if err != nil {
			tx.Rollback()
			return &userv3.User{}, err
		}

		user, groupsBefore, err := s.deleteGroupAccountRelations(ctx, tx, usr.ID, user)
		if err != nil {
			tx.Rollback()
			return &userv3.User{}, err
		}

		user, rolesAfter, err := s.createUserRoleRelations(ctx, tx, user, parsedIds{Id: usr.ID, Partner: partnerId, Organization: organizationId})
		if err != nil {
			tx.Rollback()
			return &userv3.User{}, err
		}

		user, groupsAfter, err := s.createGroupAccountRelations(ctx, tx, usr.ID, user)
		if err != nil {
			tx.Rollback()
			return &userv3.User{}, err
		}

		err = tx.Commit()
		if err != nil {
			tx.Rollback()
			_log.Warn("unable to commit changes", err)
		}

		CreateUserAuditEvent(ctx, s.db, AuditActionUpdate, user.GetMetadata().GetName(), usr.ID, rolesBefore, rolesAfter, groupsBefore, groupsAfter)
		return user, nil

	} else {
		return &userv3.User{}, fmt.Errorf("unable to update user '%v'", name)
	}

}

func (s *userService) Delete(ctx context.Context, user *userv3.User) (*userrpcv3.DeleteUserResponse, error) {
	name := user.GetMetadata().GetName()
	entity, err := dao.GetIdByTraits(ctx, s.db, name, &models.KratosIdentities{})
	if err != nil {
		return &userrpcv3.DeleteUserResponse{}, fmt.Errorf("no user founnd with username '%v'", name)
	}

	if usr, ok := entity.(*models.KratosIdentities); ok {

		tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
		if err != nil {
			return &userrpcv3.DeleteUserResponse{}, err
		}

		rolesBefore, err := s.deleteUserRoleRelations(ctx, tx, usr.ID, user)
		if err != nil {
			tx.Rollback()
			return &userrpcv3.DeleteUserResponse{}, err
		}

		user, groupsBefore, err := s.deleteGroupAccountRelations(ctx, tx, usr.ID, user)
		if err != nil {
			tx.Rollback()
			return &userrpcv3.DeleteUserResponse{}, fmt.Errorf("unable to delete user; %v", err)
		}

		err = s.ap.Delete(ctx, usr.ID.String())
		if err != nil {
			tx.Rollback()
			return &userrpcv3.DeleteUserResponse{}, err
		}

		err = tx.Commit()
		if err != nil {
			tx.Rollback()
			_log.Warn("unable to commit changes", err)
		}

		CreateUserAuditEvent(ctx, s.db, AuditActionDelete, user.GetMetadata().GetName(), usr.ID, rolesBefore, []uuid.UUID{}, groupsBefore, []uuid.UUID{})
		return &userrpcv3.DeleteUserResponse{}, nil
	}
	return &userrpcv3.DeleteUserResponse{}, fmt.Errorf("unable to delete user '%v'", user.Metadata.Name)

}

func (s *userService) List(ctx context.Context, opts ...query.Option) (*userv3.UserList, error) {
	var users []*userv3.User
	userList := &userv3.UserList{
		ApiVersion: apiVersion,
		Kind:       userListKind,
		Metadata: &v3.ListMetadata{
			Count: 0,
		},
	}

	queryOptions := commonv3.QueryOptions{}
	for _, opt := range opts {
		opt(&queryOptions)
	}

	partnerId, orgId, err := getPartnerOrganization(ctx, s.db, queryOptions.Partner, queryOptions.Organization)
	if err != nil {
		return &userv3.UserList{}, fmt.Errorf("unable to find role partner and org")
	}

	roleName := queryOptions.Role
	roleId := uuid.Nil
	if roleName != "" {
		role, err := dao.GetIdByName(ctx, s.db, roleName, &models.Role{})
		if err != nil {
			return &userv3.UserList{}, fmt.Errorf("unable to find role '%v'", roleName)
		}
		if rle, ok := role.(*models.Role); ok {
			roleId = rle.ID
		}
	}

	groupName := queryOptions.Group
	groupId := uuid.Nil
	if groupName != "" {
		group, err := dao.GetIdByName(ctx, s.db, groupName, &models.Group{})
		if err != nil {
			return &userv3.UserList{}, fmt.Errorf("unable to find group '%v'", groupName)
		}
		if grp, ok := group.(*models.Group); ok {
			groupId = grp.ID
		}
	}

	projectIds := []uuid.UUID{}
	if queryOptions.Project != "" {
		for _, p := range strings.Split(queryOptions.Project, ",") {
			if p == "ALL" {
				projectIds = append(projectIds, uuid.Nil)
			} else {
				project, err := dao.GetIdByName(ctx, s.db, p, &models.Project{})
				if err != nil {
					return &userv3.UserList{}, fmt.Errorf("unable to find project '%v'", p)
				}
				if prj, ok := project.(*models.Project); ok {
					projectIds = append(projectIds, prj.ID)
				}
			}
		}
	}

	var usrs *[]models.KratosIdentities
	var accs []models.KratosIdentities
	if len(projectIds) != 0 || groupId != uuid.Nil || roleId != uuid.Nil {
		uids, err := dao.GetQueryFilteredUsers(ctx, s.db, partnerId, orgId, groupId, roleId, projectIds)
		if err != nil {
			return &userv3.UserList{}, err
		}

		if len(uids) != 0 {
			// TODO: maybe merge this with the previous one into single sql
			usrs, err = dao.ListFilteredUsers(ctx, s.db, &accs,
				uids, queryOptions.Q, queryOptions.Type,
				queryOptions.OrderBy, queryOptions.Order,
				int(queryOptions.Limit), int(queryOptions.Offset))
			if err != nil {
				return userList, err
			}
		}
	} else {
		// If no filters are available we have to list just using identities table
		usrs, err = dao.ListFilteredUsers(ctx, s.db, &accs,
			[]uuid.UUID{}, queryOptions.Q, queryOptions.Type,
			queryOptions.OrderBy, queryOptions.Order,
			int(queryOptions.Limit), int(queryOptions.Offset))
		if err != nil {
			return userList, err
		}
	}

	if usrs != nil {
		for _, usr := range *usrs {
			user := &userv3.User{}
			user, err := s.identitiesModelToUser(ctx, s.db, user, &usr)
			if err != nil {
				return userList, err
			}
			users = append(users, user)
		}
	}

	// update the list metadata and items response
	userList.Metadata = &v3.ListMetadata{
		Count: int64(len(users)),
	}
	userList.Items = users

	return userList, nil
}

func (s *userService) RetrieveCliConfig(ctx context.Context, req *userrpcv3.ApiKeyRequest) (*common.CliConfigDownloadData, error) {
	// get the default project associated to this account
	ap, err := dao.GetDefaultAccountProject(ctx, s.db, uuid.MustParse(req.Id))
	if err != nil {
		return nil, err
	}
	// fetch the metadata information required to populate cli config
	var proj models.Project
	_, err = dao.GetByID(ctx, s.db, ap.ProjectId, &proj)
	if err != nil {
		return nil, err
	}

	var org models.Organization
	_, err = dao.GetByID(ctx, s.db, ap.OrganizationId, &org)
	if err != nil {
		return nil, err
	}

	var part models.Partner
	_, err = dao.GetByID(ctx, s.db, ap.PartnerId, &part)
	if err != nil {
		return nil, err
	}

	// get the api key if exists, if not create a new one
	apikey, err := s.ks.Get(ctx, &userrpcv3.ApiKeyRequest{Username: req.Username})
	if err != nil {
		return nil, err
	}

	if apikey == nil {
		apikey, err = s.ks.Create(ctx, &userrpcv3.ApiKeyRequest{Username: req.Username, Id: req.Id})
		if err != nil {
			return nil, err
		}
	}

	cliConfig := &common.CliConfigDownloadData{
		Profile:      s.cc.Profile,
		RestEndpoint: s.cc.RestEndpoint,
		OpsEndpoint:  s.cc.OpsEndpoint,
		ApiKey:       apikey.Key,
		ApiSecret:    apikey.Secret,
		Project:      proj.Name,
		Organization: org.Name,
		Partner:      part.Name,
	}

	return cliConfig, nil

}
