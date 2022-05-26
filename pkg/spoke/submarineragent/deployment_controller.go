package submarineragent

import (
	"context"
	"fmt"
	"strings"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/stolostron/submariner-addon/pkg/addon"
	submarinerv1alpha1 "github.com/submariner-io/submariner-operator/api/submariner/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	appsv1informers "k8s.io/client-go/informers/apps/v1"
	appsv1lister "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
	addonclient "open-cluster-management.io/api/client/addon/clientset/versioned"
)

const (
	subscriptionName           = "submariner"
	operatorName               = "submariner-operator"
	gatewayName                = "submariner-gateway"
	routeAgentName             = "submariner-routeagent"
	globalnetName              = "submariner-globalnet"
	networkPluginSyncerName    = "submariner-networkplugin-syncer"
	lighthouseAgentName        = "submariner-lighthouse-agent"
	lighthouseCoreDNSName      = "submariner-lighthouse-coredns"
	networkPluginOVNKubernetes = "OVNKubernetes"
)

const submarinerAgentDegraded = "SubmarinerAgentDegraded"

// deploymentStatusController watches the status of submariner deployments and submariner daemonsets
// on the managed cluster and reports the status to the submariner-addon on the hub cluster.
type deploymentStatusController struct {
	addOnClient        addonclient.Interface
	daemonSetLister    appsv1lister.DaemonSetLister
	deploymentLister   appsv1lister.DeploymentLister
	subscriptionLister cache.GenericLister
	submarinerLister   cache.GenericLister
	clusterName        string
	namespace          string
}

// NewDeploymentStatusController returns an instance of deploymentStatusController.
func NewDeploymentStatusController(clusterName string, installationNamespace string, addOnClient addonclient.Interface,
	daemonsetInformer appsv1informers.DaemonSetInformer, deploymentInformer appsv1informers.DeploymentInformer,
	subscriptionInformer informers.GenericInformer, submarinerInformer informers.GenericInformer, recorder events.Recorder,
) factory.Controller {
	c := &deploymentStatusController{
		addOnClient:        addOnClient,
		daemonSetLister:    daemonsetInformer.Lister(),
		deploymentLister:   deploymentInformer.Lister(),
		subscriptionLister: subscriptionInformer.Lister(),
		submarinerLister:   submarinerInformer.Lister(),
		clusterName:        clusterName,
		namespace:          installationNamespace,
	}

	return factory.New().
		WithInformers(subscriptionInformer.Informer(), daemonsetInformer.Informer(), deploymentInformer.Informer()).
		WithInformersQueueKeyFunc(func(obj runtime.Object) string {
			key, _ := cache.MetaNamespaceKeyFunc(obj)

			return key
		}, submarinerInformer.Informer()).
		WithSync(c.sync).
		ToController("SubmarinerAgentStatusController", recorder)
}

func (c *deploymentStatusController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	degradedConditionReasons := []string{}
	degradedConditionMessages := []string{}

	runtimeSub, err := c.subscriptionLister.ByNamespace(c.namespace).Get(subscriptionName)
	if errors.IsNotFound(err) {
		// submariner subscription is not found, could be deleted, ignore it.
		return nil
	}

	if err != nil {
		return err
	}

	unstructuredSub, err := runtime.DefaultUnstructuredConverter.ToUnstructured(runtimeSub)
	if err != nil {
		return err
	}

	sub := &operatorsv1alpha1.Subscription{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredSub, &sub); err != nil {
		return err
	}

	if sub.Status.InstalledCSV == "" {
		startingCSV := sub.Spec.StartingCSV
		if startingCSV == "" {
			startingCSV = "default"
		}

		channel := sub.Spec.Channel
		if channel == "" {
			channel = "default"
		}

		degradedConditionReasons = append(degradedConditionReasons, "CSVNotInstalled")
		degradedConditionMessages = append(degradedConditionMessages,
			fmt.Sprintf("The submariner-operator CSV (%s) is not installed from channel (%s) in catalog source (%s/%s)",
				startingCSV, channel, sub.Spec.CatalogSourceNamespace, sub.Spec.CatalogSource))
	}

	err = c.checkDeployments(&degradedConditionReasons, &degradedConditionMessages)
	if err != nil {
		return err
	}

	err = c.checkDaemonSets(&degradedConditionReasons, &degradedConditionMessages)
	if err != nil {
		return err
	}

	submariner, err := c.getSubmariner(syncCtx.QueueKey())

	if submariner == nil {
		return err
	}

	err = c.checkOptionalDeployments(submariner, &degradedConditionReasons, &degradedConditionMessages)
	if err != nil {
		return err
	}

	submarinerAgentCondtion := metav1.Condition{
		Type:    submarinerAgentDegraded,
		Status:  metav1.ConditionFalse,
		Reason:  "SubmarinerAgentDeployed",
		Message: fmt.Sprintf("Submariner (%s) is deployed on managed cluster.", sub.Status.InstalledCSV),
	}

	if len(degradedConditionReasons) != 0 {
		submarinerAgentCondtion.Status = metav1.ConditionTrue
		submarinerAgentCondtion.Reason = strings.Join(degradedConditionReasons, ",")
		submarinerAgentCondtion.Message = strings.Join(degradedConditionMessages, "\n")
	}

	// check submariner agent status and update submariner-addon status on the hub cluster
	updatedStatus, updated, err := addon.UpdateStatus(ctx, c.addOnClient, c.clusterName, addon.UpdateConditionFn(&submarinerAgentCondtion))
	if err != nil {
		return err
	}

	if updated {
		syncCtx.Recorder().Eventf("ManagedClusterAddOnStatusUpdated", "Updated status conditions:  %#v",
			updatedStatus.Conditions)
	}

	return nil
}

func (c *deploymentStatusController) checkDeployment(name, reasonName string, degradedConditionReasons,
	degradedConditionMessages *[]string,
) error {
	deployment, err := c.deploymentLister.Deployments(c.namespace).Get(name)
	msgName := strings.ReplaceAll(name, "-", " ")

	switch {
	case errors.IsNotFound(err):
		*degradedConditionReasons = append(*degradedConditionReasons, fmt.Sprintf("No%sDeployment", reasonName))
		*degradedConditionMessages = append(*degradedConditionMessages, fmt.Sprintf("The %s deployment does not exist", msgName))
	case err == nil:
		if deployment.Status.AvailableReplicas == 0 {
			*degradedConditionReasons = append(*degradedConditionReasons, fmt.Sprintf("No%sAvailable", reasonName))
			*degradedConditionMessages = append(*degradedConditionMessages, fmt.Sprintf("There are no %s replica available", msgName))
		}
	case err != nil:
		return err
	}

	return nil
}

func (c *deploymentStatusController) checkDeployments(degradedConditionReasons, degradedConditionMessages *[]string) error {
	err := c.checkDeployment(operatorName, "Operator", degradedConditionReasons, degradedConditionMessages)
	if err != nil {
		return err
	}

	err = c.checkDeployment(lighthouseAgentName, "LighthouseAgent", degradedConditionReasons, degradedConditionMessages)
	if err != nil {
		return err
	}

	err = c.checkDeployment(lighthouseCoreDNSName, "LighthouseCoreDNS", degradedConditionReasons, degradedConditionMessages)
	if err != nil {
		return err
	}

	return nil
}

func (c *deploymentStatusController) checkOptionalDeployments(submariner *submarinerv1alpha1.Submariner,
	degradedConditionReasons, degradedConditionMessages *[]string,
) (err error) {
	if submariner.Spec.GlobalCIDR != "" {
		err = c.checkDeployment(globalnetName, "Globalnet", degradedConditionReasons, degradedConditionMessages)
		if err != nil {
			return err
		}
	}

	if submariner.Status.NetworkPlugin == networkPluginOVNKubernetes {
		err = c.checkDeployment(networkPluginSyncerName, "NetworkPluginSyncer", degradedConditionReasons, degradedConditionMessages)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *deploymentStatusController) checkGatewayDaemonSet(degradedConditionReasons, degradedConditionMessages *[]string) error {
	gateways, err := c.daemonSetLister.DaemonSets(c.namespace).Get(gatewayName)

	switch {
	case errors.IsNotFound(err):
		*degradedConditionReasons = append(*degradedConditionReasons, "NoGatewayDaemonSet")
		*degradedConditionMessages = append(*degradedConditionMessages, "The gateway daemon set does not exist")
	case err == nil:
		if gateways.Status.DesiredNumberScheduled == 0 {
			*degradedConditionReasons = append(*degradedConditionReasons, "NoScheduledGateways")
			*degradedConditionMessages = append(*degradedConditionMessages, "There are no nodes to run the gateways")
		}

		if gateways.Status.NumberUnavailable != 0 {
			*degradedConditionReasons = append(*degradedConditionReasons, "GatewaysUnavailable")
			*degradedConditionMessages = append(*degradedConditionMessages,
				fmt.Sprintf("There are %d unavailable gateways", gateways.Status.NumberUnavailable))
		}
	case err != nil:
		return err
	}

	return nil
}

func (c *deploymentStatusController) checkRouteAgentDaemonSet(degradedConditionReasons, degradedConditionMessages *[]string) error {
	routeAgent, err := c.daemonSetLister.DaemonSets(c.namespace).Get(routeAgentName)

	switch {
	case errors.IsNotFound(err):
		*degradedConditionReasons = append(*degradedConditionReasons, "NoRouteAgentDaemonSet")
		*degradedConditionMessages = append(*degradedConditionMessages, "The route agents are not found")
	case err == nil:
		if routeAgent.Status.NumberUnavailable != 0 {
			*degradedConditionReasons = append(*degradedConditionReasons, "RouteAgentsUnavailable")
			*degradedConditionMessages = append(*degradedConditionMessages,
				fmt.Sprintf("There are %d unavailable route agents", routeAgent.Status.NumberUnavailable))
		}
	case err != nil:
		return err
	}

	return nil
}

func (c *deploymentStatusController) checkDaemonSets(degradedConditionReasons, degradedConditionMessages *[]string) error {
	err := c.checkGatewayDaemonSet(degradedConditionReasons, degradedConditionMessages)
	if err != nil {
		return err
	}

	err = c.checkRouteAgentDaemonSet(degradedConditionReasons, degradedConditionMessages)
	if err != nil {
		return err
	}

	return nil
}

func (c *deploymentStatusController) getSubmariner(queueKey string) (*submarinerv1alpha1.Submariner, error) {
	namespace, name, _ := cache.SplitMetaNamespaceKey(queueKey)
	runtimeSubmariner, err := c.submarinerLister.ByNamespace(namespace).Get(name)
	if errors.IsNotFound(err) {
		// submariner cr is not found, could be deleted, ignore it.
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	unstructuredSubmariner, err := runtime.DefaultUnstructuredConverter.ToUnstructured(runtimeSubmariner)
	if err != nil {
		return nil, err
	}

	submariner := &submarinerv1alpha1.Submariner{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredSubmariner, &submariner); err != nil {
		return nil, err
	}

	return submariner, nil
}
