package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	operatorv1 "github.com/operator-framework/api/pkg/operators/v1"
	operatorv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/spf13/viper"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/redhat-openshift-ecosystem/openshift-preflight/certification"
	"github.com/redhat-openshift-ecosystem/openshift-preflight/certification/artifacts"
	pflterr "github.com/redhat-openshift-ecosystem/openshift-preflight/certification/errors"
	"github.com/redhat-openshift-ecosystem/openshift-preflight/certification/internal/bundle"
	"github.com/redhat-openshift-ecosystem/openshift-preflight/certification/internal/cli"
	"github.com/redhat-openshift-ecosystem/openshift-preflight/certification/internal/openshift"

	log "github.com/sirupsen/logrus"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
)

type operatorData struct {
	CatalogImage     string
	Channel          string
	PackageName      string
	App              string
	InstallNamespace string
	TargetNamespace  string
	InstallModes     map[string]bool
	CsvNamespaces    []string
	InstalledCsv     string
}

type DeployableByOlmCheck struct {
	OperatorSdkEngine cli.OperatorSdkEngine
	openshiftClient   openshift.Client
	client            crclient.Client
	csvReady          bool
	validImages       bool
}

func (p *DeployableByOlmCheck) initClient() error {
	if p.client != nil {
		return nil
	}
	scheme := apiruntime.NewScheme()
	if err := openshift.AddSchemes(scheme); err != nil {
		log.Error(fmt.Errorf("%w: could not add new schemes to client", err))
		return err
	}
	kubeconfig, err := ctrl.GetConfig()
	if err != nil {
		log.Error("could not get kubeconfig")
		return err
	}

	client, err := crclient.New(kubeconfig, crclient.Options{
		Scheme: scheme,
	})
	if err != nil {
		log.Error("could not get controller-runtime client")
		return err
	}

	p.client = client
	return nil
}

func (p *DeployableByOlmCheck) initOpenShifeEngine() error {
	if p.openshiftClient == nil {
		p.openshiftClient = openshift.NewClient(p.client)
	}
	return nil
}

func NewDeployableByOlmCheck(operatorSdkEngine *cli.OperatorSdkEngine) *DeployableByOlmCheck {
	return &DeployableByOlmCheck{
		OperatorSdkEngine: *operatorSdkEngine,
	}
}

func (p *DeployableByOlmCheck) Validate(ctx context.Context, bundleRef certification.ImageReference) (bool, error) {
	p.initClient()
	p.initOpenShifeEngine()
	if report, err := bundle.Validate(ctx, p.OperatorSdkEngine, bundleRef.ImageFSPath); err != nil || !report.Passed {
		return false, err
	}

	// gather the list of registry and pod images
	beforeOperatorImages, err := p.getImages(ctx)
	if err != nil {
		return false, err
	}

	// retrieve the required data
	operatorData, err := p.operatorMetadata(ctx, bundleRef)
	if err != nil {
		return false, err
	}

	log.Debugf("The operator Metadata is %+v", *operatorData)

	// create k8s custom resources for the operator deployment
	err = p.setUp(ctx, operatorData)
	defer p.cleanUp(ctx, *operatorData)

	if err != nil {
		return false, err
	}

	installedCSV, err := p.installedCSV(ctx, *operatorData)
	if err != nil {
		return false, err
	}
	operatorData.InstalledCsv = installedCSV
	log.Trace("the installed CSV is ", operatorData.InstalledCsv)

	p.csvReady, err = p.isCSVReady(ctx, *operatorData)
	if err != nil {
		return false, err
	}

	afterOperatorImages, err := p.getImages(ctx)
	if err != nil {
		return false, err
	}

	operatorImages := diffImageList(beforeOperatorImages, afterOperatorImages)
	p.validImages = checkImageSource(operatorImages)

	return p.csvReady, nil
}

func diffImageList(before, after map[string]struct{}) []string {
	var operatorImages []string
	for image := range after {
		if _, ok := before[image]; !ok {
			operatorImages = append(operatorImages, image)
		}
	}
	return operatorImages
}

func checkImageSource(operatorImages []string) bool {
	log.Debug("Checking that images are from approved sources...")

	registries := make([]string, 0, len(approvedRegistries))
	for registry := range approvedRegistries {
		registries = append(registries, registry)
	}

	log.Debug("List of approved registries are: ", registries)
	allApproved := true
	for _, image := range operatorImages {
		userRegistry := strings.Split(image, "/")[0]
		if _, ok := approvedRegistries[userRegistry]; !ok {
			log.Warnf("Unapproved registry found for image %s", image)
			allApproved = false
		}
	}
	if allApproved {
		log.Debug("All images are from approved sources...")
	}
	return allApproved
}

func (p *DeployableByOlmCheck) operatorMetadata(ctx context.Context, bundleRef certification.ImageReference) (*operatorData, error) {
	// retrieve the operator metadata from bundle image
	annotationsFileName := filepath.Join(bundleRef.ImageFSPath, "metadata", "annotations.yaml")
	annotationsFile, err := os.Open(annotationsFileName)
	if err != nil {
		log.Error(fmt.Errorf("%w: could not open annotations.yaml", err))
		return nil, err
	}
	annotations, err := bundle.GetAnnotations(ctx, annotationsFile)
	if err != nil {
		log.Error("unable to get annotations.yaml from the bundle")
		return nil, err
	}

	catalogImage := viper.GetString(indexImageKey)

	channel, err := annotation(annotations, channelKeyInBundle)
	if err != nil {
		log.Error("unable to extract channel name from the bundle: ", err)
		return nil, err
	}

	if len(viper.GetString(channelKey)) != 0 {
		channel = viper.GetString(channelKey)
	}

	packageName, err := annotation(annotations, packageKey)
	if err != nil {
		log.Error("unable to extract package name from the bundle: ", err)
		return nil, err
	}

	csvFilepath, err := bundle.GetCsvFilePathFromBundle(bundleRef.ImageFSPath)
	if err != nil {
		return nil, err
	}

	csvFileReader, err := os.Open(csvFilepath)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	installModes, err := bundle.GetSupportedInstallModes(ctx, csvFileReader)
	if err != nil {
		log.Error(fmt.Errorf("%w: unable to extract operator install modes from ClusterServiceVersion", err))
		return nil, err
	}

	return &operatorData{
		CatalogImage:     catalogImage,
		Channel:          channel,
		PackageName:      packageName,
		App:              packageName,
		InstallNamespace: packageName,
		TargetNamespace:  packageName + "-target",
		InstallModes:     installModes,
	}, nil
}

func (p *DeployableByOlmCheck) setUp(ctx context.Context, operatorData *operatorData) error {
	if _, err := p.openshiftClient.CreateNamespace(ctx, operatorData.InstallNamespace); err != nil && !k8serr.IsAlreadyExists(err) {
		return err
	}

	if _, err := p.openshiftClient.CreateNamespace(ctx, operatorData.TargetNamespace); err != nil && !k8serr.IsAlreadyExists(err) {
		return err
	}

	dockerconfig := viper.GetString("dockerConfig")
	if len(dockerconfig) != 0 {
		content, err := p.readFileAsByteArray(dockerconfig)
		if err != nil {
			return err
		}
		data := map[string]string{".dockerconfigjson": string(content)}
		if _, err := p.openshiftClient.CreateSecret(ctx, secretName, data, corev1.SecretTypeDockerConfigJson, operatorData.InstallNamespace); err != nil && !k8serr.IsAlreadyExists(err) {
			return err
		}
	} else {
		log.Debug("No docker config file is found to access the index image in private registries. Proceeding...")
	}

	if strings.Contains(operatorData.CatalogImage, imageRegistryService) {
		indexImageNamespace := strings.Split(operatorData.CatalogImage, "/")[1]
		if len(indexImageNamespace) != 0 {
			// create rolebindings for the pipeline service account
			if err := p.grantRegistryPermissionToServiceAccount(ctx, pipelineServiceAccount, operatorData.InstallNamespace,
				indexImageNamespace); err != nil {
				return err
			}
			// create rolebinding for the default OperatorHub catalog sources
			if err := p.grantRegistryPermissionToServiceAccount(ctx, operatorData.App, openshiftMarketplaceNamespace,
				indexImageNamespace); err != nil {
				return err
			}
			// create rolebindings for the custom catalog
			if err := p.grantRegistryPermissionToServiceAccount(ctx, operatorData.App, operatorData.InstallNamespace,
				indexImageNamespace); err != nil {
				return err
			}

		}
	}

	if _, err := p.openshiftClient.CreateCatalogSource(ctx, openshift.CatalogSourceData{Name: operatorData.App, Image: operatorData.CatalogImage, Secrets: []string{secretName}}, operatorData.InstallNamespace); err != nil && !k8serr.IsAlreadyExists(err) {
		return err
	}

	operatorGroupData, err := p.generateOperatorGroupData(operatorData)
	if err != nil {
		return err
	}
	if _, err := p.openshiftClient.CreateOperatorGroup(ctx, operatorGroupData, operatorData.InstallNamespace); err != nil && !k8serr.IsAlreadyExists(err) {
		return err
	}

	subscriptionData := openshift.SubscriptionData{
		Name:                   operatorData.App,
		Channel:                operatorData.Channel,
		CatalogSource:          operatorData.App,
		CatalogSourceNamespace: operatorData.InstallNamespace,
		Package:                operatorData.PackageName,
	}
	if _, err := p.openshiftClient.CreateSubscription(ctx, subscriptionData, operatorData.InstallNamespace); err != nil && !k8serr.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (p *DeployableByOlmCheck) generateOperatorGroupData(operatorData *operatorData) (openshift.OperatorGroupData, error) {
	var installMode string
	for i := 0; i < len(prioritizedInstallModes); i++ {
		if _, ok := operatorData.InstallModes[prioritizedInstallModes[i]]; ok {
			installMode = prioritizedInstallModes[i]
			break
		}
	}
	log.Debugf("The operator install mode is %s", installMode)
	targetNamespaces := make([]string, 2)

	switch installMode {
	case string(operatorv1alpha1.InstallModeTypeOwnNamespace):
		targetNamespaces = []string{operatorData.InstallNamespace}
	case string(operatorv1alpha1.InstallModeTypeSingleNamespace):
		targetNamespaces = []string{operatorData.TargetNamespace}
	case string(operatorv1alpha1.InstallModeTypeMultiNamespace):
		targetNamespaces = []string{operatorData.TargetNamespace, operatorData.InstallNamespace}
	case string(operatorv1alpha1.InstallModeTypeAllNamespaces):
		targetNamespaces = []string{}

	}
	log.Debugf("The OperatorGroup's TargetNamespaces is %s", targetNamespaces)
	operatorData.CsvNamespaces = targetNamespaces
	return openshift.OperatorGroupData{Name: operatorData.App, TargetNamespaces: targetNamespaces}, nil
}

func (p *DeployableByOlmCheck) grantRegistryPermissionToServiceAccount(ctx context.Context, serviceAccount, serviceAccountNamespace, indexImageNamespace string) error {
	for _, role := range []string{registryViewerRole, imagePullerRole} {
		roleBindingData := openshift.RoleBindingData{
			Name:      fmt.Sprintf("%s:%s:%s", serviceAccount, serviceAccountNamespace, role),
			Subjects:  []string{serviceAccount},
			Role:      role,
			Namespace: serviceAccountNamespace,
		}
		if _, err := p.openshiftClient.CreateRoleBinding(ctx, roleBindingData, indexImageNamespace); err != nil && !k8serr.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

type watchFunc func(ctx context.Context, client openshift.Client, name, namespace string) (string, bool, error)

func watch(ctx context.Context, client openshift.Client, wg *sync.WaitGroup, name, namespace string, timeout time.Duration, channel chan string, fn watchFunc) {
	defer wg.Done()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		log.Debugf("Waiting for object %s/%s to become ready...", namespace, name)
		obj, done, err := fn(ctx, client, name, namespace)
		if err != nil {
			// Something bad happened. Get out of town
			log.Errorf("could not retrieve the object %s/%s: %s", namespace, name, err)
			channel <- fmt.Sprintf("%s %v", errorPrefix, err)
			return
		}
		if done {
			log.Debugf("Successfully retrieved object %s/%s", namespace, obj)
			channel <- obj
			return
		}
		log.Debugf("Object %s/%s is not set yet, retrying...", namespace, name)

		select {
		case <-ctx.Done():
			log.Errorf("failed to retrieve object %s/%s: %s", namespace, name, ctx.Err())
			channel <- fmt.Sprintf("%s %v", errorPrefix, ctx.Err())
			return
		default:
			time.Sleep(2 * time.Second)
		}
	}
}

func csvStatusSucceeded(ctx context.Context, client openshift.Client, name, namespace string) (string, bool, error) {
	csv, err := client.GetCSV(ctx, name, namespace)
	if err != nil && !k8serr.IsNotFound(err) {
		// This is not a normal error. Get out of town
		log.Errorf("failed to fetch the csv %s from namespace %s: %s", name, namespace, err)
		return "", false, err
	}
	// if the CSV phase is succeeded, stop the querying
	if csv.Status.Phase == operatorv1alpha1.CSVPhaseSucceeded {
		log.Debugf("CSV %s is created successfully in namespace %s", name, namespace)
		return name, true, nil
	}
	return "", false, nil
}

func (p *DeployableByOlmCheck) isCSVReady(ctx context.Context, operatorData operatorData) (bool, error) {
	var CsvNamespaces []string
	if len(operatorData.CsvNamespaces) == 0 {
		CsvNamespaces = []string{operatorData.TargetNamespace, "default", openshiftMarketplaceNamespace}
	} else {
		CsvNamespaces = []string{operatorData.CsvNamespaces[0]}
	}
	log.Tracef("Looking for csv %s in namespace(s) %s", operatorData.InstalledCsv, CsvNamespaces)

	csvChannel := make(chan string)

	var wg sync.WaitGroup

	for _, CsvNamespace := range CsvNamespaces {
		wg.Add(1)
		go watch(ctx, p.openshiftClient, &wg, operatorData.InstalledCsv, CsvNamespace, csvTimeout, csvChannel, csvStatusSucceeded)
	}

	go func() {
		wg.Wait()
		close(csvChannel)
	}()

	for msg := range csvChannel {
		if strings.Contains(msg, errorPrefix) {
			return false, fmt.Errorf("%w: %s", pflterr.ErrK8sAPICallFailed, msg)
		}
		if len(msg) == 0 {
			return false, nil
		}
	}
	return true, nil
}

func subscriptionCsvIsInstalled(ctx context.Context, client openshift.Client, name, namespace string) (string, bool, error) {
	sub, err := client.GetSubscription(ctx, name, namespace)
	if err != nil && !k8serr.IsNotFound(err) {
		log.Errorf("failed to fetch the subscription %s from namespace %s: %s", name, namespace, err)
		return "", false, err
	}
	log.Tracef("current subscription status is %+v", sub.Status)
	installedCSV := sub.Status.InstalledCSV
	// if the installedCSV field is present, stop the querying
	if len(installedCSV) > 0 {
		return installedCSV, true, nil
	}
	return "", false, nil
}

func (p *DeployableByOlmCheck) installedCSV(ctx context.Context, operatorData operatorData) (string, error) {
	installedCSVChannel := make(chan string)

	var wg sync.WaitGroup
	// query API server for the installed CSV field of the created subscription
	wg.Add(1)
	go watch(ctx, p.openshiftClient, &wg, operatorData.App, operatorData.InstallNamespace, subscriptionTimeout, installedCSVChannel, subscriptionCsvIsInstalled)

	go func() {
		wg.Wait()
		close(installedCSVChannel)
	}()

	installedCsv := ""
	for msg := range installedCSVChannel {
		if strings.Contains(msg, errorPrefix) {
			return "", fmt.Errorf("%w: %s", pflterr.ErrK8sAPICallFailed, msg)
		}
		installedCsv = msg
	}

	return installedCsv, nil
}

func (p *DeployableByOlmCheck) cleanUp(ctx context.Context, operatorData operatorData) {
	log.Debug("Dumping data in artifacts/ directory")

	subs, err := p.openshiftClient.GetSubscription(ctx, operatorData.App, operatorData.InstallNamespace)
	if err != nil {
		log.Warn("unable to retrieve the subscription")
	} else {
		p.writeToFile(subs)
	}

	cs, err := p.openshiftClient.GetCatalogSource(ctx, operatorData.App, operatorData.InstallNamespace)
	if err != nil {
		log.Warn("unable to retrieve the catalogsource")
	} else {
		p.writeToFile(cs)
	}

	og, err := p.openshiftClient.GetOperatorGroup(ctx, operatorData.App, operatorData.InstallNamespace)
	if err != nil {
		log.Warn("unable to retrieve the operatorgroup")
	} else {
		p.writeToFile(og)
	}

	installNamespace, err := p.openshiftClient.GetNamespace(ctx, operatorData.InstallNamespace)
	if err != nil {
		log.Warn("unable to retrieve the install namespace")
	} else {
		p.writeToFile(installNamespace)
	}

	targetNamespace, err := p.openshiftClient.GetNamespace(ctx, operatorData.TargetNamespace)
	if err != nil {
		log.Warn("unable to retrieve the target namespace")
	} else {
		p.writeToFile(targetNamespace)
	}

	log.Trace("Deleting the resources created by DeployableByOLM Check")
	p.openshiftClient.DeleteSubscription(ctx, operatorData.App, operatorData.InstallNamespace)
	p.openshiftClient.DeleteCatalogSource(ctx, operatorData.App, operatorData.InstallNamespace)
	p.openshiftClient.DeleteOperatorGroup(ctx, operatorData.App, operatorData.InstallNamespace)
	p.openshiftClient.DeleteSecret(ctx, secretName, operatorData.InstallNamespace)

	if strings.Contains(operatorData.CatalogImage, imageRegistryService) {
		indexImageNamespace := strings.Split(operatorData.CatalogImage, "/")[1]
		operatorServiceAccount := operatorData.App
		operatorNamespace := operatorData.InstallNamespace
		// remove pipeline-related rolebindings
		p.openshiftClient.DeleteRoleBinding(ctx, fmt.Sprintf("%s:%s:%s", pipelineServiceAccount, operatorNamespace, registryViewerRole), indexImageNamespace)
		p.openshiftClient.DeleteRoleBinding(ctx, fmt.Sprintf("%s:%s:%s", pipelineServiceAccount, operatorNamespace, imagePullerRole), indexImageNamespace)
		// remove rolebindings required for the default OperatorHub catalog sources
		p.openshiftClient.DeleteRoleBinding(ctx, fmt.Sprintf("%s:%s:%s", operatorServiceAccount, openshiftMarketplaceNamespace, registryViewerRole), indexImageNamespace)
		p.openshiftClient.DeleteRoleBinding(ctx, fmt.Sprintf("%s:%s:%s", operatorServiceAccount, openshiftMarketplaceNamespace, imagePullerRole), indexImageNamespace)
		// remove rolebindings required for custom catalog sources
		p.openshiftClient.DeleteRoleBinding(ctx, fmt.Sprintf("%s:%s:%s", operatorServiceAccount, operatorNamespace, registryViewerRole), indexImageNamespace)
		p.openshiftClient.DeleteRoleBinding(ctx, fmt.Sprintf("%s:%s:%s", operatorServiceAccount, operatorNamespace, imagePullerRole), indexImageNamespace)
	}
	p.openshiftClient.DeleteNamespace(ctx, operatorData.InstallNamespace)
	p.openshiftClient.DeleteNamespace(ctx, operatorData.TargetNamespace)
}

func (p *DeployableByOlmCheck) writeToFile(data interface{}) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(data)
	if err != nil {
		log.Error(fmt.Errorf("%w: unable to convert the object to unstructured.Unstructured", err))
	}

	group := "operators.coreos.com"
	var version, kind string
	u := &unstructured.Unstructured{Object: obj}
	switch data.(type) {
	case *operatorv1alpha1.CatalogSource:
		version = "v1alpha1"
		kind = "CatalogSource"
	case *operatorv1.OperatorGroup:
		version = "v1"
		kind = "OperatorGroup"
	case *operatorv1alpha1.Subscription:
		version = "v1alpha1"
		kind = "Subscription"
	case *corev1.Namespace:
		group = ""
		version = "v1"
		kind = "Namespace"
	default:
		return pflterr.ErrUnsupportedGoType
	}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   group,
		Version: version,
		Kind:    kind,
	})

	jsonManifest, err := json.Marshal(u)
	if err != nil {
		log.Error("unable to marshal to json: ", err)
		return err
	}

	filename := fmt.Sprintf("%s-%s.json", u.GetName(), u.GetKind())
	if _, err := artifacts.WriteFile(filename, string(jsonManifest)); err != nil {
		log.Error("failed to write the k8s object to the file", err)
		return err
	}
	return nil
}

func (p *DeployableByOlmCheck) readFileAsByteArray(filename string) ([]byte, error) {
	content, err := os.ReadFile(filename)
	if err != nil {
		log.Errorf("error reading the file: %s", filename)
		return nil, err
	}
	return content, nil
}

func (p *DeployableByOlmCheck) getImages(ctx context.Context) (map[string]struct{}, error) {
	return p.openshiftClient.GetImages(ctx)
}

func (p *DeployableByOlmCheck) Name() string {
	return "DeployableByOLM"
}

func (p *DeployableByOlmCheck) Metadata() certification.Metadata {
	return certification.Metadata{
		Description:      "Checking if the operator could be deployed by OLM",
		Level:            "best",
		KnowledgeBaseURL: "https://sdk.operatorframework.io/docs/olm-integration/testing-deployment/",
		CheckURL:         "https://sdk.operatorframework.io/docs/olm-integration/testing-deployment/",
	}
}

func (p *DeployableByOlmCheck) Help() certification.HelpText {
	return certification.HelpText{
		Message:    "It is required that your operator could be deployed by OLM",
		Suggestion: "Follow the guidelines on the operatorsdk website to learn how to package your operator https://sdk.operatorframework.io/docs/olm-integration/cli-overview/",
	}
}
