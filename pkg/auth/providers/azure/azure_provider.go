// Package azure provides functions and types to register and use Azure AD as the auth provider in Rancher.
package azure

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	lru "github.com/hashicorp/golang-lru"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
	"github.com/rancher/norman/httperror"
	"github.com/rancher/norman/types"
	v32 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/auth/providers/azure/clients"
	"github.com/rancher/rancher/pkg/auth/providers/common"
	"github.com/rancher/rancher/pkg/auth/tokens"
	client "github.com/rancher/rancher/pkg/client/generated/management/v3"
	publicclient "github.com/rancher/rancher/pkg/client/generated/management/v3public"
	corev1 "github.com/rancher/rancher/pkg/generated/norman/core/v1"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/types/config"
	"github.com/rancher/rancher/pkg/user"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	Name = clients.Name
)

type azureProvider struct {
	ctx         context.Context
	authConfigs v3.AuthConfigInterface
	secrets     corev1.SecretInterface
	userMGR     user.Manager
	tokenMGR    *tokens.Manager
}

func Configure(ctx context.Context, mgmtCtx *config.ScaledContext, userMGR user.Manager, tokenMGR *tokens.Manager) common.AuthProvider {
	var err error
	clients.GroupCache, err = lru.New(settings.AzureGroupCacheSize.GetInt())
	if err != nil {
		logrus.Warnf("initial azure-group-cache-size was invalid value, setting to 10000 error:%v", err)
		clients.GroupCache, _ = lru.New(10000)
	}

	return &azureProvider{
		ctx:         ctx,
		authConfigs: mgmtCtx.Management.AuthConfigs(""),
		secrets:     mgmtCtx.Core.Secrets(""),
		userMGR:     userMGR,
		tokenMGR:    tokenMGR,
	}
}

func (ap *azureProvider) GetName() string {
	return Name
}

func (ap *azureProvider) AuthenticateUser(ctx context.Context, input interface{}) (v3.Principal, []v3.Principal, string, error) {
	login, ok := input.(*v32.AzureADLogin)
	if !ok {
		return v3.Principal{}, nil, "", errors.New("unexpected input type")
	}
	cfg, err := ap.getAzureConfigK8s()
	if err != nil {
		return v3.Principal{}, nil, "", err
	}
	return ap.loginUser(cfg, login, false)
}

func (ap *azureProvider) RefetchGroupPrincipals(principalID, secret string) ([]v3.Principal, error) {
	cfg, err := ap.getAzureConfigK8s()
	if err != nil {
		return nil, err
	}
	var useDeprecatedAzureADClient = graphEndpointDeprecated(cfg.GraphEndpoint)
	azureClient, err := clients.NewAzureClientFromSecret(cfg, useDeprecatedAzureADClient, secret, ap.secrets)
	if err != nil {
		return nil, err
	}

	logrus.Debug("[AZURE_PROVIDER] Started getting user info from AzureAD")

	parsed, err := clients.ParsePrincipalID(principalID)
	if err != nil {
		return nil, err
	}
	userPrincipal, err := azureClient.GetUser(parsed["ID"])
	if err != nil {
		return nil, err
	}

	logrus.Debug("[AZURE_PROVIDER] Completed getting user info from AzureAD")

	userGroups, err := azureClient.ListGroupMemberships(clients.GetPrincipalID(userPrincipal))
	if err != nil {
		return nil, err
	}

	groupPrincipals, err := clients.UserGroupsToPrincipals(azureClient, userGroups)
	if err != nil {
		return nil, err
	}

	return groupPrincipals, nil
}

func (ap *azureProvider) SearchPrincipals(name, principalType string, token v3.Token) ([]v3.Principal, error) {
	cfg, err := ap.getAzureConfigK8s()
	if err != nil {
		return nil, err
	}
	var principals []v3.Principal

	azureClient, err := ap.newAzureClientFromToken(cfg, &token)
	if err != nil {
		return nil, err
	}

	switch principalType {
	case "user":
		principals, err = ap.searchUserPrincipalsByName(azureClient, name, token)
		if err != nil {
			return nil, err
		}
	case "group":
		principals, err = ap.searchGroupPrincipalsByName(azureClient, name, token)
		if err != nil {
			return nil, err
		}
	case "":
		users, err := ap.searchUserPrincipalsByName(azureClient, name, token)
		if err != nil {
			return nil, err
		}
		groups, err := ap.searchGroupPrincipalsByName(azureClient, name, token)
		if err != nil {
			return nil, err
		}
		principals = append(principals, users...)
		principals = append(principals, groups...)
	}
	return principals, ap.updateToken(azureClient, &token)
}

func (ap *azureProvider) GetPrincipal(principalID string, token v3.Token) (v3.Principal, error) {
	var principal v3.Principal
	var err error
	cfg, err := ap.getAzureConfigK8s()
	if err != nil {
		return v3.Principal{}, err
	}

	azureClient, err := ap.newAzureClientFromToken(cfg, &token)
	if err != nil {
		return principal, err
	}

	parsed, err := clients.ParsePrincipalID(principalID)
	if err != nil {
		return v3.Principal{}, httperror.NewAPIError(httperror.NotFound, "invalid principal")
	}

	switch parsed["type"] {
	case "user":
		principal, err = ap.getUserPrincipal(azureClient, parsed["ID"], token)
	case "group":
		principal, err = ap.getGroupPrincipal(azureClient, parsed["ID"], token)
	}

	if err != nil {
		return v3.Principal{}, err
	}

	return principal, ap.updateToken(azureClient, &token)
}

func (ap *azureProvider) CustomizeSchema(schema *types.Schema) {
	schema.ActionHandler = ap.actionHandler
	schema.Formatter = ap.formatter
}

func (ap *azureProvider) TransformToAuthProvider(
	authConfig map[string]interface{},
) (map[string]interface{}, error) {
	p := common.TransformToAuthProvider(authConfig)
	p[publicclient.AzureADProviderFieldRedirectURL] = formAzureRedirectURL(authConfig)
	return p, nil
}

func (ap *azureProvider) loginUser(config *v32.AzureADConfig, azureCredential *v32.AzureADLogin, test bool) (v3.Principal, []v3.Principal, string, error) {
	var useDeprecatedAzureADClient = graphEndpointDeprecated(config.GraphEndpoint)
	azureClient, err := clients.NewAzureClientFromCredential(config, useDeprecatedAzureADClient, azureCredential, ap.secrets)
	if err != nil {
		return v3.Principal{}, nil, "", err
	}
	userPrincipal, groupPrincipals, providerToken, err := azureClient.LoginUser(config, azureCredential)
	if err != nil {
		return v3.Principal{}, nil, "", err
	}
	testAllowedPrincipals := config.AllowedPrincipalIDs
	if test && config.AccessMode == "restricted" {
		testAllowedPrincipals = append(testAllowedPrincipals, userPrincipal.Name)
	}

	allowed, err := ap.userMGR.CheckAccess(config.AccessMode, testAllowedPrincipals, userPrincipal.Name, groupPrincipals)
	if err != nil {
		return v3.Principal{}, nil, "", err
	}
	if !allowed {
		return v3.Principal{}, nil, "", httperror.NewAPIError(httperror.Unauthorized, "unauthorized")
	}

	return userPrincipal, groupPrincipals, providerToken, nil
}

func (ap *azureProvider) getUserPrincipal(client clients.AzureClient, principalID string, token v3.Token) (v3.Principal, error) {
	principal, err := client.GetUser(principalID)
	if err != nil {
		return v3.Principal{}, httperror.NewAPIError(httperror.NotFound, err.Error())
	}
	principal.Me = samePrincipal(token.UserPrincipal, principal)
	return principal, nil
}

func (ap *azureProvider) getGroupPrincipal(client clients.AzureClient, id string, token v3.Token) (v3.Principal, error) {
	principal, err := client.GetGroup(id)
	if err != nil {
		return v3.Principal{}, httperror.NewAPIError(httperror.NotFound, err.Error())
	}
	principal.MemberOf = ap.tokenMGR.IsMemberOf(token, principal)
	return principal, nil
}

func (ap *azureProvider) searchUserPrincipalsByName(client clients.AzureClient, name string, token v3.Token) ([]v3.Principal, error) {
	filter := fmt.Sprintf("startswith(userPrincipalName,'%[1]s') or startswith(displayName,'%[1]s') or startswith(givenName,'%[1]s') or startswith(surname,'%[1]s')", name)
	principals, err := client.ListUsers(filter)
	if err != nil {
		return nil, err
	}
	for _, principal := range principals {
		principal.Me = samePrincipal(token.UserPrincipal, principal)
	}
	return principals, nil
}

func (ap *azureProvider) searchGroupPrincipalsByName(client clients.AzureClient, name string, token v3.Token) ([]v3.Principal, error) {
	filter := fmt.Sprintf("startswith(displayName,'%[1]s') or startswith(mail,'%[1]s') or startswith(mailNickname,'%[1]s')", name)
	principals, err := client.ListGroups(filter)
	if err != nil {
		return nil, err
	}
	for _, principal := range principals {
		principal.MemberOf = ap.tokenMGR.IsMemberOf(token, principal)
	}
	return principals, nil
}

func (ap *azureProvider) newAzureClientFromToken(cfg *v32.AzureADConfig, token *v32.Token) (clients.AzureClient, error) {
	var secret string
	var deprecated = graphEndpointDeprecated(cfg.GraphEndpoint)
	if deprecated {
		var err error
		secret, err = ap.tokenMGR.GetSecret(token.UserID, Name, []*v3.Token{token})
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	return clients.NewAzureClientFromSecret(cfg, deprecated, secret, ap.secrets)
}

func (ap *azureProvider) saveAzureConfigK8s(config *v32.AzureADConfig) error {
	storedAzureConfig, err := ap.getAzureConfigK8s()
	if err != nil {
		return err
	}
	config.APIVersion = "management.cattle.io/v3"
	config.Kind = v3.AuthConfigGroupVersionKind.Kind
	config.Type = client.AzureADConfigType
	config.ObjectMeta = storedAzureConfig.ObjectMeta

	field := strings.ToLower(client.AzureADConfigFieldApplicationSecret)
	if err := common.CreateOrUpdateSecrets(ap.secrets, config.ApplicationSecret, field, strings.ToLower(config.Type)); err != nil {
		return err
	}

	config.ApplicationSecret = common.GetName(config.Type, field)

	logrus.Debugf("updating AzureADConfig")
	_, err = ap.authConfigs.ObjectClient().Update(config.ObjectMeta.Name, config)
	if err != nil {
		return err
	}
	return nil
}

func (ap *azureProvider) getAzureConfigK8s() (*v32.AzureADConfig, error) {
	authConfigObj, err := ap.authConfigs.ObjectClient().UnstructuredClient().Get(Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve AzureADConfig, error: %v", err)
	}

	u, ok := authConfigObj.(runtime.Unstructured)
	if !ok {
		return nil, fmt.Errorf("failed to retrieve AzureADConfig, cannot read k8s Unstructured data")
	}
	storedAzureADConfigMap := u.UnstructuredContent()

	storedAzureADConfig := &v32.AzureADConfig{}
	if err := mapstructure.Decode(storedAzureADConfigMap, storedAzureADConfig); err != nil {
		return nil, err
	}

	metadataMap, ok := storedAzureADConfigMap["metadata"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("failed to retrieve AzureADConfig metadata, cannot read k8s Unstructured data")
	}

	objectMeta := &metav1.ObjectMeta{}
	mapstructure.Decode(metadataMap, objectMeta)
	storedAzureADConfig.ObjectMeta = *objectMeta

	if storedAzureADConfig.ApplicationSecret != "" {
		value, err := common.ReadFromSecret(ap.secrets, storedAzureADConfig.ApplicationSecret,
			strings.ToLower(client.AzureADConfigFieldApplicationSecret))
		if err != nil {
			return nil, err
		}
		storedAzureADConfig.ApplicationSecret = value
	}

	return storedAzureADConfig, nil
}

// updateToken compares the current Azure access token to the one stored in the secret and updates if needed.
// This is relevant only for access tokens to the deprecated Azure AD Graph API.
func (ap *azureProvider) updateToken(client clients.AzureClient, token *v3.Token) error {
	// For the new flow via Microsoft Graph, the caching and updating of the token to the Microsoft Graph API
	// is handled separately via the SDK client cache.
	cfg, err := ap.getAzureConfigK8s()
	if err != nil {
		return err
	}
	if !graphEndpointDeprecated(cfg.GraphEndpoint) {
		return nil
	}

	current, err := client.MarshalTokenJSON()
	if err != nil {
		return errors.New("failed to unmarshal token")
	}

	secret, err := ap.tokenMGR.GetSecret(token.UserID, token.AuthProvider, []*v3.Token{token})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// providerToken doesn't exist as a secret, update on token.
			if current, ok := token.ProviderInfo["access_token"]; ok && current != current {
				token.ProviderInfo["access_token"] = current
			}
			return nil
		}
		return err
	}

	if current == secret {
		return nil
	}

	return ap.tokenMGR.UpdateSecret(token.UserID, token.AuthProvider, current)
}

func formAzureRedirectURL(config map[string]interface{}) string {
	var ac v32.AzureADConfig
	if err := mapstructure.Decode(config, &ac); err == nil {
		if !graphEndpointDeprecated(ac.GraphEndpoint) {
			// Return the redirect URL for Microsoft Graph.
			return fmt.Sprintf(
				"%s?client_id=%s&redirect_uri=%s&response_type=code&scope=openid",
				ac.AuthEndpoint,
				ac.ApplicationID,
				ac.RancherURL,
			)
		}
	} else {
		logrus.Warnf("failed to determine if Graph endpoint is deprecated when generating redirect URL: %v", err)
	}
	// Return the redirect URL for the deprecated Azure AD Graph.
	return fmt.Sprintf(
		"%s?client_id=%s&redirect_uri=%s&resource=%s&scope=openid",
		config["authEndpoint"],
		config["applicationId"],
		config["rancherUrl"],
		config["graphEndpoint"],
	)
}

func (ap *azureProvider) CanAccessWithGroupProviders(userPrincipalID string, groupPrincipals []v3.Principal) (bool, error) {
	cfg, err := ap.getAzureConfigK8s()
	if err != nil {
		logrus.Errorf("Error fetching azure config: %v", err)
		return false, err
	}
	allowed, err := ap.userMGR.CheckAccess(cfg.AccessMode, cfg.AllowedPrincipalIDs, userPrincipalID, groupPrincipals)
	if err != nil {
		return false, err
	}
	return allowed, nil
}

func samePrincipal(me v3.Principal, other v3.Principal) bool {
	if me.ObjectMeta.Name == other.ObjectMeta.Name && me.LoginName == other.LoginName && me.PrincipalType == other.PrincipalType {
		return true
	}
	return false
}

// UpdateGroupCacheSize attempts to update the size of the group cache defined at the package level.
func UpdateGroupCacheSize(size string) {
	i, err := strconv.Atoi(size)
	if err != nil {
		logrus.Errorf("error parsing azure-group-cache-size, skipping update %v", err)
		return
	}
	if i < 0 {
		logrus.Error("azure-group-cache-size must be >= 0, skipping update")
		return
	}
	clients.GroupCache.Resize(i)
}

func (ap *azureProvider) GetUserExtraAttributes(userPrincipal v3.Principal) map[string][]string {
	extras := make(map[string][]string)
	if userPrincipal.Name != "" {
		extras[common.UserAttributePrincipalID] = []string{userPrincipal.Name}
	}
	if userPrincipal.LoginName != "" {
		extras[common.UserAttributeUserName] = []string{userPrincipal.LoginName}
	}
	return extras
}
