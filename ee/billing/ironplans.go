// +build ee

package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/porter-dev/porter/api/types"
	"github.com/porter-dev/porter/ee/models"
	"github.com/porter-dev/porter/ee/repository"
	"gorm.io/gorm"

	cemodels "github.com/porter-dev/porter/internal/models"
)

// Client contains an API client for IronPlans
type Client struct {
	apiKey    string
	serverURL string
	repo      repository.EERepository

	httpClient *http.Client

	defaultPlanID string
	customPlanID  string
}

// NewClient creates a new billing API client
func NewClient(serverURL, apiKey string, repo repository.EERepository) (*Client, error) {
	httpClient := &http.Client{
		Timeout: time.Minute,
	}

	client := &Client{apiKey, serverURL, repo, httpClient, "", ""}

	// get the default plans from the IronPlans API server
	defPlanID, err := client.GetExistingPublicPlan("Free")

	if err != nil {
		return nil, err
	}

	customPlanID, err := client.GetExistingPublicPlan("Enterprise")

	if err != nil {
		return nil, err
	}

	client.defaultPlanID = defPlanID
	client.customPlanID = customPlanID

	return client, nil
}

func (c *Client) CreateTeam(proj *cemodels.Project) (string, error) {
	resp := &Team{}
	err := c.postRequest("/teams/v1", &CreateTeamRequest{
		Name: proj.Name,
	}, resp)

	if err != nil {
		return "", err
	}

	// put the user on the free plan, as the default behavior, if there is a default plan
	if c.defaultPlanID != "" {
		err = c.CreateOrUpdateSubscription(resp.ID, c.defaultPlanID)

		if err != nil {
			return "", fmt.Errorf("subscription creation failed: %s", err)
		}
	}

	_, err = c.repo.ProjectBilling().CreateProjectBilling(&models.ProjectBilling{
		ProjectID:     proj.ID,
		BillingTeamID: resp.ID,
	})

	if err != nil {
		return "", err
	}

	return resp.ID, err
}

func (c *Client) DeleteTeam(proj *cemodels.Project) error {
	projBilling, err := c.repo.ProjectBilling().ReadProjectBillingByProjectID(proj.ID)

	if err != nil {
		return err
	}

	return c.deleteRequest(fmt.Sprintf("/teams/v1/%s", projBilling.BillingTeamID), nil, nil)
}

func (c *Client) GetTeamID(proj *cemodels.Project) (teamID string, err error) {
	projBilling, err := c.repo.ProjectBilling().ReadProjectBillingByProjectID(proj.ID)

	if err != nil {
		return "", err
	}

	return projBilling.BillingTeamID, nil
}

func (c *Client) CreatePlan(teamID string, proj *cemodels.Project, planSpec *types.AddProjectBillingRequest) (string, error) {
	// construct basic plan object
	planFeatures := make([]*CreatePlanFeature, 0)

	userDisplay := fmt.Sprintf("Up to %d users", planSpec.Users)

	if planSpec.Users == 0 {
		userDisplay = fmt.Sprintf("Unlimited users")
	}

	clusterDisplay := fmt.Sprintf("Up to %d clusters", planSpec.Clusters)

	if planSpec.Clusters == 0 {
		clusterDisplay = fmt.Sprintf("Unlimited clusters")
	}

	cpuDisplay := fmt.Sprintf("Up to %d CPUs", planSpec.CPU)

	if planSpec.CPU == 0 {
		cpuDisplay = fmt.Sprintf("Unlimited CPU")
	}

	ramDisplay := fmt.Sprintf("Up to %d GB RAM", planSpec.Memory)

	if planSpec.Memory == 0 {
		ramDisplay = fmt.Sprintf("Unlimited RAM")
	}

	planFeatures = append(planFeatures, &CreatePlanFeature{
		Display: userDisplay,
	})
	planFeatures = append(planFeatures, &CreatePlanFeature{
		Display: clusterDisplay,
	})
	planFeatures = append(planFeatures, &CreatePlanFeature{
		Display: cpuDisplay,
	})
	planFeatures = append(planFeatures, &CreatePlanFeature{
		Display: ramDisplay,
	})

	var customPlanID *string

	if c.customPlanID != "" {
		customPlanID = &c.customPlanID
	}

	createPlanReq := &CreatePlanRequest{
		Name:               proj.Name,
		IsActive:           true,
		IsPublic:           false,
		IsTrialAllowed:     true,
		ReplacePlanID:      customPlanID,
		PerMonthPriceCents: planSpec.Price,
		PerYearPriceCents:  12 * planSpec.Price,
		Features:           planFeatures,
		TeamsAccess: []*CreatePlanTeamsAccess{
			{
				TeamID: teamID,
				Revoke: false,
			},
		},
	}

	// find all relevant feature IDs
	listResp := &ListFeaturesResponse{}
	err := c.getRequest("/features/v1", listResp)

	if err != nil {
		return "", err
	}

	// create a feature spec per feature ID, and add to features array for plan
	for _, feature := range listResp.Results {
		featureSpec := &CreateFeatureSpecRequest{
			Name:         "unnamed",
			RecordPeriod: "monthly",
			Aggregation:  "sum",
			UnitPrice:    0,
		}

		switch feature.Slug {
		case FeatureSlugUsers:
			featureSpec.MaxLimit = planSpec.Users
			featureSpec.UnitsIncluded = planSpec.Users
		case FeatureSlugClusters:
			featureSpec.MaxLimit = planSpec.Clusters
			featureSpec.UnitsIncluded = planSpec.Clusters
		case FeatureSlugCPU:
			featureSpec.MaxLimit = planSpec.CPU
			featureSpec.UnitsIncluded = planSpec.CPU
		case FeatureSlugMemory:
			featureSpec.MaxLimit = planSpec.Memory
			featureSpec.UnitsIncluded = planSpec.Memory
		// continue on default behavior so that feature spec is not created for
		// features that don't match a slug
		default:
			continue
		}

		// create the feature spec
		resp := &CreateFeaturespecResponse{}
		err = c.postRequest("/featurespecs/v1/", featureSpec, resp)

		if err != nil {
			return "", err
		}

		var index int
		switch feature.Slug {
		case FeatureSlugUsers:
			index = 0
		case FeatureSlugClusters:
			index = 1
		case FeatureSlugCPU:
			index = 2
		case FeatureSlugMemory:
			index = 3
		}

		createPlanReq.Features[index].FeatureID = feature.ID
		createPlanReq.Features[index].SpecID = resp.ID
	}

	// create the plan and return the plan ID
	planResp := &Plan{}

	err = c.postRequest("/plans/v1/", createPlanReq, planResp)

	if err != nil {
		return "", err
	}

	return planResp.ID, nil
}

func (c *Client) CreateOrUpdateSubscription(teamID, planID string) error {
	// determine if subscription already exists by reading the team ID and seeing if the subscription
	// field has an ID attached
	teamResp := &Team{}
	err := c.getRequest(fmt.Sprintf("/teams/v1/%s", teamID), teamResp)

	if err != nil {
		return err
	}

	subReq := &CreateSubscriptionRequest{
		PlanID:     planID,
		NextPlanID: c.defaultPlanID,
		TeamID:     teamID,
		IsPaused:   false,
	}

	// if subscription ID is not empty, perform a PUT request to update the subscription
	if teamResp.Subscription.ID != "" {
		// delete the subscription
		err = c.deleteRequest(fmt.Sprintf("/subscriptions/v1/%s/purge/", teamResp.Subscription.ID), nil, nil)

		if err != nil {
			return err
		}
	}

	return c.postRequest("/subscriptions/v1", subReq, nil)
}

func (c *Client) GetExistingPublicPlan(planName string) (string, error) {
	listResp := &ListPlansResponse{}
	err := c.getRequest("/plans/v1/", listResp, map[string]string{"is_public": "true"})

	if err != nil {
		return "", err
	}

	for _, plan := range listResp.Results {
		if plan.Name == planName {
			return plan.ID, nil
		}
	}

	return "", fmt.Errorf("plan not found")
}

func (c *Client) AddUserToTeam(teamID string, user *cemodels.User, role *cemodels.Role) error {
	// determine if user is already in team/has user billing
	userBilling, err := c.repo.UserBilling().ReadUserBilling(role.ProjectID, user.ID)

	if userBilling != nil {
		return nil
	}

	roleEnum := RoleEnumMember

	// if user's role is admin, add them to the team as an owner
	if role.Kind == types.RoleAdmin {
		roleEnum = RoleEnumOwner
	}

	req := &AddTeammateRequest{
		TeamID:   teamID,
		Role:     roleEnum,
		Email:    user.Email,
		SourceID: fmt.Sprintf("%d-%d", role.ProjectID, user.ID),
	}

	resp := &Teammate{}

	err = c.postRequest("/team_memberships/v1", req, resp)

	if err != nil {
		return err
	}

	_, err = c.repo.UserBilling().CreateUserBilling(&models.UserBilling{
		ProjectID:  role.ProjectID,
		UserID:     user.ID,
		TeammateID: resp.ID,
		Token:      []byte(""),
	})

	return err
}

func (c *Client) UpdateUserInTeam(role *cemodels.Role) error {
	// get the user billing information to get the membership id
	userBilling, err := c.repo.UserBilling().ReadUserBilling(role.ProjectID, role.UserID)

	if err != nil {
		return err
	}

	roleEnum := RoleEnumMember

	// if user's role is admin, add them to the team as an owner
	if role.Kind == types.RoleAdmin {
		roleEnum = RoleEnumOwner
	}

	req := &UpdateTeammateRequest{
		Role: roleEnum,
	}

	resp := &Teammate{}

	return c.putRequest(fmt.Sprintf("/team_memberships/v1/%s", userBilling.TeammateID), req, resp)
}

func (c *Client) RemoveUserFromTeam(role *cemodels.Role) error {
	// get the user billing information to get the membership id
	userBilling, err := c.repo.UserBilling().ReadUserBilling(role.ProjectID, role.UserID)

	if err != nil {
		return err
	}

	return c.deleteRequest(fmt.Sprintf("/team_memberships/v1/%s", userBilling.TeammateID), nil, nil)
}

// GetIDToken gets an id token for a user in a project, creating the ID token if necessary
func (c *Client) GetIDToken(proj *cemodels.Project, user *cemodels.User) (token string, teamID string, err error) {
	// attempt to get a team ID for the project
	teamID, err = c.GetTeamID(proj)

	// attempt to read the user billing data from the project
	userBilling, err := c.repo.UserBilling().ReadUserBilling(proj.ID, user.ID)
	notFound := errors.Is(err, gorm.ErrRecordNotFound)

	if !notFound && err != nil {
		return "", "", err
	}

	if !notFound {
		token = string(userBilling.Token)

		if token != "" {
			// check if the JWT token has expired
			isTokExpired := isExpired(token)

			// if JWT token has not expired, return the token
			if !isTokExpired {
				return token, teamID, nil
			}
		}
	}

	req := &CreateIDTokenRequest{
		Email:  user.Email,
		UserID: fmt.Sprintf("%d-%d", proj.ID, user.ID),
	}

	resp := &CreateIDTokenResponse{}

	err = c.postRequest("/customers/v1/token", req, resp)

	if err != nil {
		return "", "", err
	}

	token = resp.Token

	if notFound {
		_, err := c.repo.UserBilling().CreateUserBilling(&models.UserBilling{
			ProjectID: proj.ID,
			UserID:    user.ID,
			Token:     []byte(token),
		})

		if err != nil {
			return "", "", err
		}
	} else {
		_, err := c.repo.UserBilling().UpdateUserBilling(&models.UserBilling{
			Model: &gorm.Model{
				ID: userBilling.ID,
			},
			ProjectID:  proj.ID,
			UserID:     user.ID,
			Token:      []byte(token),
			TeammateID: userBilling.TeammateID,
		})

		if err != nil {
			return "", "", err
		}
	}

	return token, teamID, nil
}

// VerifySignature verifies a webhook signature based on hmac protocol
// https://docs.ironplans.com/webhook-events/webhook-events
func (c *Client) VerifySignature(signature string, body []byte) bool {
	if len(signature) != 71 || !strings.HasPrefix(signature, "sha256=") {
		return false
	}

	actual := make([]byte, 32)
	_, err := hex.Decode(actual, []byte(signature[7:]))

	if err != nil {
		return false
	}

	computed := hmac.New(sha256.New, []byte(c.apiKey))
	_, err = computed.Write(body)

	if err != nil {
		return false
	}

	return hmac.Equal(computed.Sum(nil), actual)
}

func (c *Client) postRequest(path string, data interface{}, dst interface{}) error {
	return c.writeRequest("POST", path, data, dst)
}

func (c *Client) putRequest(path string, data interface{}, dst interface{}) error {
	return c.writeRequest("PUT", path, data, dst)
}

func (c *Client) deleteRequest(path string, data interface{}, dst interface{}) error {
	return c.writeRequest("DELETE", path, data, dst)
}

func (c *Client) getRequest(path string, dst interface{}, query ...map[string]string) error {
	reqURL, err := url.Parse(c.serverURL)

	if err != nil {
		return nil
	}

	reqURL.Path = path

	q := reqURL.Query()
	for _, queryGroup := range query {
		for key, val := range queryGroup {
			q.Add(key, val)
		}
	}

	reqURL.RawQuery = q.Encode()

	req, err := http.NewRequest(
		"GET",
		reqURL.String(),
		nil,
	)

	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json; charset=utf-8")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))

	res, err := c.httpClient.Do(req)

	if err != nil {
		return err
	}

	defer res.Body.Close()

	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusBadRequest {
		resBytes, err := ioutil.ReadAll(res.Body)

		if err != nil {
			return fmt.Errorf("request failed with status code %d, but could not read body (%s)\n", res.StatusCode, err.Error())
		}

		return fmt.Errorf("request failed with status code %d: %s\n", res.StatusCode, string(resBytes))
	}

	if dst != nil {
		return json.NewDecoder(res.Body).Decode(dst)
	}

	return nil
}

func (c *Client) writeRequest(method, path string, data interface{}, dst interface{}) error {
	reqURL, err := url.Parse(c.serverURL)

	if err != nil {
		return nil
	}

	reqURL.Path = path

	var strData []byte

	if data != nil {
		strData, err = json.Marshal(data)

		if err != nil {
			return err
		}
	}

	req, err := http.NewRequest(
		method,
		reqURL.String(),
		strings.NewReader(string(strData)),
	)

	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json; charset=utf-8")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))

	res, err := c.httpClient.Do(req)

	if err != nil {
		return err
	}

	defer res.Body.Close()

	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusBadRequest {
		resBytes, err := ioutil.ReadAll(res.Body)

		if err != nil {
			return fmt.Errorf("request failed with status code %d, but could not read body (%s)\n", res.StatusCode, err.Error())
		}

		return fmt.Errorf("request failed with status code %d: %s\n", res.StatusCode, string(resBytes))
	}

	if dst != nil {
		return json.NewDecoder(res.Body).Decode(dst)
	}

	return nil
}

const (
	FeatureSlugCPU      string = "cpu"
	FeatureSlugMemory   string = "memory"
	FeatureSlugClusters string = "clusters"
	FeatureSlugUsers    string = "users"
)

func (c *Client) ParseProjectUsageFromWebhook(payload []byte) (*cemodels.ProjectUsage, error) {
	subscription := &SubscriptionWebhookRequest{}

	err := json.Unmarshal(payload, subscription)

	if err != nil {
		return nil, err
	}

	// if event type is not subscription, return wrong webhook event type error
	if subscription.EventType != "subscription" {
		return nil, nil
	}

	// get the project id linked to that team
	projBilling, err := c.repo.ProjectBilling().ReadProjectBillingByTeamID(subscription.TeamID)

	if err != nil {
		return nil, err
	}

	usage := &cemodels.ProjectUsage{
		ProjectID: projBilling.ProjectID,
	}

	for _, feature := range subscription.Plan.Features {
		// look for slug of "cpus" and "memory"
		maxLimit := uint(feature.FeatureSpec.MaxLimit)
		switch feature.Feature.Slug {
		case FeatureSlugCPU:
			usage.ResourceCPU = maxLimit
		case FeatureSlugMemory:
			usage.ResourceMemory = 1000 * maxLimit
		case FeatureSlugClusters:
			usage.Clusters = maxLimit
		case FeatureSlugUsers:
			usage.Users = maxLimit
		}
	}

	return usage, nil
}

type expiryJWT struct {
	ExpiresAt int64 `json:"exp"`
}

func isExpired(token string) bool {
	var encoded string

	if tokenSplit := strings.Split(token, "."); len(tokenSplit) != 3 {
		return true
	} else {
		encoded = tokenSplit[1]
	}

	decodedBytes, err := base64.RawStdEncoding.DecodeString(encoded)

	if err != nil {
		return true
	}

	expiryData := &expiryJWT{}

	err = json.Unmarshal(decodedBytes, expiryData)

	if err != nil {
		return true
	}

	expiryTime := time.Unix(expiryData.ExpiresAt, 0)

	return expiryTime.Before(time.Now())
}
