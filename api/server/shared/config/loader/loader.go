package loader

import (
	"fmt"
	"net/http"
	"strconv"

	gorillaws "github.com/gorilla/websocket"
	"github.com/porter-dev/porter/api/server/shared/apierrors/alerter"
	"github.com/porter-dev/porter/api/server/shared/config"
	"github.com/porter-dev/porter/api/server/shared/config/env"
	"github.com/porter-dev/porter/api/server/shared/config/envloader"
	"github.com/porter-dev/porter/api/server/shared/websocket"
	"github.com/porter-dev/porter/internal/adapter"
	"github.com/porter-dev/porter/internal/analytics"
	"github.com/porter-dev/porter/internal/auth/sessionstore"
	"github.com/porter-dev/porter/internal/auth/token"
	"github.com/porter-dev/porter/internal/billing"
	"github.com/porter-dev/porter/internal/helm/urlcache"
	"github.com/porter-dev/porter/internal/integrations/powerdns"
	"github.com/porter-dev/porter/internal/kubernetes"
	"github.com/porter-dev/porter/internal/kubernetes/local"
	"github.com/porter-dev/porter/internal/notifier"
	"github.com/porter-dev/porter/internal/notifier/sendgrid"
	"github.com/porter-dev/porter/internal/oauth"
	"github.com/porter-dev/porter/internal/repository/credentials"
	"github.com/porter-dev/porter/internal/repository/gorm"

	lr "github.com/porter-dev/porter/internal/logger"

	pgorm "gorm.io/gorm"
)

var InstanceBillingManager billing.BillingManager
var InstanceEnvConf *envloader.EnvConf
var InstanceDB *pgorm.DB
var InstanceCredentialBackend credentials.CredentialStorage

type EnvConfigLoader struct {
	version string
}

func NewEnvLoader(version string) config.ConfigLoader {
	return &EnvConfigLoader{version}
}

func sharedInit() {
	var err error
	InstanceEnvConf, _ = envloader.FromEnv()

	InstanceDB, err = adapter.New(InstanceEnvConf.DBConf)

	if err != nil {
		panic(err)
	}

	InstanceBillingManager = &billing.NoopBillingManager{}
}

func (e *EnvConfigLoader) LoadConfig() (res *config.Config, err error) {
	envConf := InstanceEnvConf
	sc := envConf.ServerConf

	res = &config.Config{
		Logger:            lr.NewConsole(sc.Debug),
		ServerConf:        sc,
		DBConf:            envConf.DBConf,
		RedisConf:         envConf.RedisConf,
		BillingManager:    InstanceBillingManager,
		CredentialBackend: InstanceCredentialBackend,
	}

	res.Metadata = config.MetadataFromConf(envConf.ServerConf, e.version)
	res.DB = InstanceDB

	err = gorm.AutoMigrate(InstanceDB)

	if err != nil {
		return nil, err
	}

	var key [32]byte

	for i, b := range []byte(envConf.DBConf.EncryptionKey) {
		key[i] = b
	}

	res.Repo = gorm.NewRepository(InstanceDB, &key, InstanceCredentialBackend)

	// create the session store
	res.Store, err = sessionstore.NewStore(
		&sessionstore.NewStoreOpts{
			SessionRepository: res.Repo.Session(),
			CookieSecrets:     envConf.ServerConf.CookieSecrets,
		},
	)

	if err != nil {
		return nil, err
	}

	res.TokenConf = &token.TokenGeneratorConf{
		TokenSecret: envConf.ServerConf.TokenGeneratorSecret,
	}

	res.UserNotifier = &notifier.EmptyUserNotifier{}

	if res.Metadata.Email {
		res.UserNotifier = sendgrid.NewUserNotifier(&sendgrid.Client{
			APIKey:                  envConf.ServerConf.SendgridAPIKey,
			PWResetTemplateID:       envConf.ServerConf.SendgridPWResetTemplateID,
			PWGHTemplateID:          envConf.ServerConf.SendgridPWGHTemplateID,
			VerifyEmailTemplateID:   envConf.ServerConf.SendgridVerifyEmailTemplateID,
			ProjectInviteTemplateID: envConf.ServerConf.SendgridProjectInviteTemplateID,
			SenderEmail:             envConf.ServerConf.SendgridSenderEmail,
		})
	}

	res.Alerter = alerter.NoOpAlerter{}

	if envConf.ServerConf.SentryDSN != "" {
		res.Alerter, err = alerter.NewSentryAlerter(envConf.ServerConf.SentryDSN, envConf.ServerConf.SentryEnv)
	}

	if sc.DOClientID != "" && sc.DOClientSecret != "" {
		res.DOConf = oauth.NewDigitalOceanClient(&oauth.Config{
			ClientID:     sc.DOClientID,
			ClientSecret: sc.DOClientSecret,
			Scopes:       []string{"read", "write"},
			BaseURL:      sc.ServerURL,
		})
	}

	if sc.GoogleClientID != "" && sc.GoogleClientSecret != "" {
		res.GoogleConf = oauth.NewGoogleClient(&oauth.Config{
			ClientID:     sc.GoogleClientID,
			ClientSecret: sc.GoogleClientSecret,
			Scopes: []string{
				"openid",
				"profile",
				"email",
			},
			BaseURL: sc.ServerURL,
		})
	}

	if sc.GithubClientID != "" && sc.GithubClientSecret != "" {
		res.GithubConf = oauth.NewGithubClient(&oauth.Config{
			ClientID:     sc.GithubClientID,
			ClientSecret: sc.GithubClientSecret,
			Scopes:       []string{"read:user", "user:email"},
			BaseURL:      sc.ServerURL,
		})
	}

	if sc.GithubAppClientID != "" &&
		sc.GithubAppClientSecret != "" &&
		sc.GithubAppName != "" &&
		sc.GithubAppWebhookSecret != "" &&
		sc.GithubAppSecretPath != "" &&
		sc.GithubAppID != "" {
		if AppID, err := strconv.ParseInt(sc.GithubAppID, 10, 64); err == nil {
			res.GithubAppConf = oauth.NewGithubAppClient(&oauth.Config{
				ClientID:     sc.GithubAppClientID,
				ClientSecret: sc.GithubAppClientSecret,
				Scopes:       []string{"read:user"},
				BaseURL:      sc.ServerURL,
			}, sc.GithubAppName, sc.GithubAppWebhookSecret, sc.GithubAppSecretPath, AppID)
		}
	}

	if sc.SlackClientID != "" && sc.SlackClientSecret != "" {
		res.SlackConf = oauth.NewSlackClient(&oauth.Config{
			ClientID:     sc.SlackClientID,
			ClientSecret: sc.SlackClientSecret,
			Scopes: []string{
				"incoming-webhook",
				"team:read",
			},
			BaseURL: sc.ServerURL,
		})
	}

	res.WSUpgrader = &websocket.Upgrader{
		WSUpgrader: &gorillaws.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				return origin == sc.ServerURL
			},
		},
	}

	// construct the whitelisted users map
	wlUsers := make(map[uint]uint)

	for _, userID := range sc.WhitelistedUsers {
		wlUsers[userID] = userID
	}

	res.WhitelistedUsers = wlUsers

	res.URLCache = urlcache.Init(sc.DefaultApplicationHelmRepoURL, sc.DefaultAddonHelmRepoURL)

	provAgent, err := getProvisionerAgent(sc)

	if err != nil {
		return nil, err
	}

	res.ProvisionerAgent = provAgent

	if res.ProvisionerAgent != nil && res.RedisConf.Enabled {
		res.Metadata.Provisioning = true
	}

	res.AnalyticsClient = analytics.InitializeAnalyticsSegmentClient(sc.SegmentClientKey, res.Logger)

	if sc.PowerDNSAPIKey != "" && sc.PowerDNSAPIServerURL != "" {
		res.PowerDNSClient = powerdns.NewClient(sc.PowerDNSAPIServerURL, sc.PowerDNSAPIKey, sc.AppRootDomain)
	}

	return res, nil
}

func getProvisionerAgent(sc *env.ServerConf) (*kubernetes.Agent, error) {
	if sc.ProvisionerCluster == "kubeconfig" && sc.SelfKubeconfig != "" {
		agent, err := local.GetSelfAgentFromFileConfig(sc.SelfKubeconfig)

		if err != nil {
			return nil, fmt.Errorf("could not get in-cluster agent: %v", err)
		}

		return agent, nil
	} else if sc.ProvisionerCluster == "kubeconfig" {
		return nil, fmt.Errorf(`"kubeconfig" cluster option requires path to kubeconfig`)
	}

	agent, _ := kubernetes.GetAgentInClusterConfig()

	return agent, nil
}
