package release

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/porter-dev/porter/api/server/authz"
	"github.com/porter-dev/porter/api/server/handlers"
	"github.com/porter-dev/porter/api/server/shared"
	"github.com/porter-dev/porter/api/server/shared/apierrors"
	"github.com/porter-dev/porter/api/server/shared/config"
	"github.com/porter-dev/porter/api/types"
	"github.com/porter-dev/porter/internal/helm"
	"github.com/porter-dev/porter/internal/helm/loader"
	"github.com/porter-dev/porter/internal/integrations/slack"
	"github.com/porter-dev/porter/internal/models"
	"helm.sh/helm/v3/pkg/release"
)

type UpgradeReleaseHandler struct {
	handlers.PorterHandlerReadWriter
	authz.KubernetesAgentGetter
}

func NewUpgradeReleaseHandler(
	config *config.Config,
	decoderValidator shared.RequestDecoderValidator,
	writer shared.ResultWriter,
) *UpgradeReleaseHandler {
	return &UpgradeReleaseHandler{
		PorterHandlerReadWriter: handlers.NewDefaultPorterHandler(config, decoderValidator, writer),
		KubernetesAgentGetter:   authz.NewOutOfClusterAgentGetter(config),
	}
}

func (c *UpgradeReleaseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, _ := r.Context().Value(types.UserScope).(*models.User)
	cluster, _ := r.Context().Value(types.ClusterScope).(*models.Cluster)
	helmRelease, _ := r.Context().Value(types.ReleaseScope).(*release.Release)

	helmAgent, err := c.GetHelmAgent(r, cluster)

	if err != nil {
		c.HandleAPIError(w, r, apierrors.NewErrInternal(err))
		return
	}

	request := &types.UpgradeReleaseRequest{}

	if ok := c.DecodeAndValidate(w, r, request); !ok {
		return
	}

	registries, err := c.Repo().Registry().ListRegistriesByProjectID(cluster.ProjectID)

	if err != nil {
		c.HandleAPIError(w, r, apierrors.NewErrInternal(err))
		return
	}

	conf := &helm.UpgradeReleaseConfig{
		Name:       helmRelease.Name,
		Cluster:    cluster,
		Repo:       c.Repo(),
		Registries: registries,
	}

	// if the chart version is set, load a chart from the repo
	if request.ChartVersion != "" {
		cache := c.Config().URLCache
		chartRepoURL, foundFirst := cache.GetURL(helmRelease.Chart.Metadata.Name)

		if !foundFirst {
			cache.Update()

			var found bool

			chartRepoURL, found = cache.GetURL(helmRelease.Chart.Metadata.Name)

			if !found {
				c.HandleAPIError(w, r, apierrors.NewErrPassThroughToClient(
					fmt.Errorf("chart not found"),
					http.StatusBadRequest,
				))

				return
			}
		}

		chart, err := loader.LoadChartPublic(
			chartRepoURL,
			helmRelease.Chart.Metadata.Name,
			request.ChartVersion,
		)

		if err != nil {
			c.HandleAPIError(w, r, apierrors.NewErrPassThroughToClient(
				fmt.Errorf("chart not found"),
				http.StatusBadRequest,
			))

			return
		}

		conf.Chart = chart
	}

	helmRelease, upgradeErr := helmAgent.UpgradeRelease(conf, request.Values, c.Config().DOConf)

	slackInts, _ := c.Repo().SlackIntegration().ListSlackIntegrationsByProjectID(cluster.ProjectID)

	rel, releaseErr := c.Repo().Release().ReadRelease(cluster.ID, helmRelease.Name, helmRelease.Namespace)

	var notifConf *models.NotificationConfigExternal
	notifConf = nil
	if rel != nil && rel.NotificationConfig != 0 {
		conf, err := c.Repo().NotificationConfig().ReadNotificationConfig(rel.NotificationConfig)

		if err != nil {
			c.HandleAPIError(w, r, apierrors.NewErrInternal(err))
			return
		}

		notifConf = conf.Externalize()
	}

	notifier := slack.NewSlackNotifier(notifConf, slackInts...)

	notifyOpts := &slack.NotifyOpts{
		ProjectID:   cluster.ProjectID,
		ClusterID:   cluster.ID,
		ClusterName: cluster.Name,
		Name:        helmRelease.Name,
		Namespace:   helmRelease.Namespace,
		URL: fmt.Sprintf(
			"%s/applications/%s/%s/%s",
			c.Config().ServerConf.ServerURL,
			url.PathEscape(cluster.Name),
			cluster.Name,
			helmRelease.Name,
		) + fmt.Sprintf("?project_id=%d", cluster.ProjectID),
	}

	if upgradeErr != nil {
		notifyOpts.Status = slack.StatusFailed
		notifyOpts.Info = upgradeErr.Error()

		notifier.Notify(notifyOpts)

		c.HandleAPIError(w, r, apierrors.NewErrPassThroughToClient(
			upgradeErr,
			http.StatusBadRequest,
		))

		return
	}

	notifyOpts.Status = string(helmRelease.Info.Status)
	notifyOpts.Version = helmRelease.Version

	notifier.Notify(notifyOpts)

	// update the github actions env if the release exists and is built from source
	if cName := helmRelease.Chart.Metadata.Name; cName == "job" || cName == "web" || cName == "worker" {
		if releaseErr == nil && rel != nil {
			err = updateReleaseRepo(c.Config(), rel, helmRelease)

			if err != nil {
				c.HandleAPIError(w, r, apierrors.NewErrInternal(err))
				return
			}

			gitAction := rel.GitActionConfig

			if gitAction != nil && gitAction.ID != 0 {
				gaRunner, err := getGARunner(
					c.Config(),
					user.ID,
					cluster.ProjectID,
					cluster.ID,
					rel.GitActionConfig,
					helmRelease.Name,
					helmRelease.Namespace,
					rel,
					helmRelease,
				)

				if err != nil {
					c.HandleAPIError(w, r, apierrors.NewErrInternal(err))
					return
				}

				err = gaRunner.CreateEnvSecret()

				if err != nil {
					c.HandleAPIError(w, r, apierrors.NewErrInternal(err))
					return
				}
			}
		}
	}
}
