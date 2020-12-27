package tests

import (
	"context"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	promv1 "github.com/coreos/prometheus-operator/pkg/apis/monitoring/v1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	secv1 "github.com/openshift/api/security/v1"
	templatev1 "github.com/openshift/api/template/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/pointer"
	lifecycleapi "kubevirt.io/controller-lifecycle-operator-sdk/pkg/sdk/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	sspv1beta1 "kubevirt.io/ssp-operator/api/v1beta1"
)

const (
	envExistingCrName        = "TEST_EXISTING_CR_NAME"
	envExistingCrNamespace   = "TEST_EXISTING_CR_NAMESPACE"
	envSkipUpdateSspTests    = "SKIP_UPDATE_SSP_TESTS"
	envSkipCleanupAfterTests = "SKIP_CLEANUP_AFTER_TESTS"

	shortTimeout = 1 * time.Minute
	timeout      = 10 * time.Minute
)

type TestSuiteStrategy interface {
	Init()
	Cleanup()

	GetName() string
	GetNamespace() string
	GetTemplatesNamespace() string
	GetValidatorReplicas() int

	RevertToOriginalSspCr()
	SkipSspUpdateTestsIfNeeded()
}

type newSspStrategy struct {
	ssp *sspv1beta1.SSP
}

var _ TestSuiteStrategy = &newSspStrategy{}

func (s *newSspStrategy) Init() {
	Eventually(func() error {
		namespaceObj := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.GetNamespace()}}
		return apiClient.Create(ctx, namespaceObj)
	}, timeout, time.Second).ShouldNot(HaveOccurred())

	Eventually(func() error {
		namespaceObj := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.GetTemplatesNamespace()}}
		return apiClient.Create(ctx, namespaceObj)
	}, timeout, time.Second).ShouldNot(HaveOccurred())

	newSsp := &sspv1beta1.SSP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.GetName(),
			Namespace: s.GetNamespace(),
		},
		Spec: sspv1beta1.SSPSpec{
			TemplateValidator: sspv1beta1.TemplateValidator{
				Replicas: pointer.Int32Ptr(int32(s.GetValidatorReplicas())),
			},
			CommonTemplates: sspv1beta1.CommonTemplates{
				Namespace: s.GetTemplatesNamespace(),
			},
		},
	}

	Eventually(func() error {
		return apiClient.Create(ctx, newSsp)
	}, timeout, time.Second).ShouldNot(HaveOccurred())
	s.ssp = newSsp
}

func (s *newSspStrategy) Cleanup() {
	if getBoolEnv(envSkipCleanupAfterTests) {
		return
	}

	if s.ssp != nil {
		err := apiClient.Delete(ctx, s.ssp)
		expectSuccessOrNotFound(err)
		waitForDeletion(client.ObjectKey{
			Name:      s.GetName(),
			Namespace: s.GetNamespace(),
		}, &sspv1beta1.SSP{})
	}

	err1 := apiClient.Delete(ctx, &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.GetNamespace()}})
	err2 := apiClient.Delete(ctx, &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.GetTemplatesNamespace()}})
	expectSuccessOrNotFound(err1)
	expectSuccessOrNotFound(err2)

	waitForDeletion(client.ObjectKey{Name: s.GetNamespace()}, &v1.Namespace{})
	waitForDeletion(client.ObjectKey{Name: s.GetTemplatesNamespace()}, &v1.Namespace{})
}

func (s *newSspStrategy) GetName() string {
	return "test-ssp"
}

func (s *newSspStrategy) GetNamespace() string {
	const testNamespace = "ssp-operator-functests"
	return testNamespace
}

func (s *newSspStrategy) GetTemplatesNamespace() string {
	const commonTemplatesTestNS = "ssp-operator-functests-templates"
	return commonTemplatesTestNS
}

func (s *newSspStrategy) GetValidatorReplicas() int {
	const templateValidatorReplicas = 2
	return templateValidatorReplicas
}

func (s *newSspStrategy) RevertToOriginalSspCr() {
	createOrUpdateSsp(s.ssp)
}

func (s *newSspStrategy) SkipSspUpdateTestsIfNeeded() {
	// Do not skip SSP update tests in this strategy
}

type existingSspStrategy struct {
	Name      string
	Namespace string

	ssp *sspv1beta1.SSP
}

var _ TestSuiteStrategy = &existingSspStrategy{}

func (s *existingSspStrategy) Init() {
	existingSsp := &sspv1beta1.SSP{}
	err := apiClient.Get(ctx, client.ObjectKey{Name: s.Name, Namespace: s.Namespace}, existingSsp)
	Expect(err).ToNot(HaveOccurred())

	templatesNamespace := existingSsp.Spec.CommonTemplates.Namespace
	Expect(apiClient.Get(ctx, client.ObjectKey{Name: templatesNamespace}, &v1.Namespace{}))

	s.ssp = existingSsp

	if s.sspModificationDisabled() {
		return
	}

	// Try to modify the SSP and check if it is not reverted by another operator
	defer s.RevertToOriginalSspCr()

	newReplicasCount := *existingSsp.Spec.TemplateValidator.Replicas + 1
	updateSsp(func(foundSsp *sspv1beta1.SSP) {
		foundSsp.Spec.TemplateValidator.Replicas = &newReplicasCount
	})

	Consistently(func() int32 {
		return *getSsp().Spec.TemplateValidator.Replicas
	}, 20*time.Second, time.Second).Should(Equal(newReplicasCount),
		"The SSP CR was modified outside of the test. "+
			"If the CR is managed by a controller, consider disabling modification tests by setting "+
			"SKIP_UPDATE_SSP_TESTS=true")
}

func (s *existingSspStrategy) Cleanup() {
	if s.ssp != nil {
		s.RevertToOriginalSspCr()
	}
}

func (s *existingSspStrategy) GetName() string {
	return s.Name
}

func (s *existingSspStrategy) GetNamespace() string {
	return s.Namespace
}

func (s *existingSspStrategy) GetTemplatesNamespace() string {
	if s.ssp == nil {
		panic("Strategy is not initialized")
	}
	return s.ssp.Spec.CommonTemplates.Namespace
}

func (s *existingSspStrategy) GetValidatorReplicas() int {
	if s.ssp == nil {
		panic("Strategy is not initialized")
	}
	return int(*s.ssp.Spec.TemplateValidator.Replicas)
}

func (s *existingSspStrategy) RevertToOriginalSspCr() {
	createOrUpdateSsp(s.ssp)
}

func (s *existingSspStrategy) SkipSspUpdateTestsIfNeeded() {
	if s.sspModificationDisabled() {
		Skip("Tests that update SSP CR are disabled", 1)
	}
}

func (s *existingSspStrategy) sspModificationDisabled() bool {
	return getBoolEnv(envSkipUpdateSspTests)
}

var (
	apiClient          client.Client
	ctx                context.Context
	strategy           TestSuiteStrategy
	sspListerWatcher   cache.ListerWatcher
	deploymentTimedOut bool
)

var _ = BeforeSuite(func() {
	existingCrName := os.Getenv(envExistingCrName)
	if existingCrName == "" {
		strategy = &newSspStrategy{}
	} else {
		existingCrNamespace := os.Getenv(envExistingCrNamespace)
		Expect(existingCrNamespace).ToNot(BeEmpty(), "Existing CR Namespace needs to be defined")
		strategy = &existingSspStrategy{Name: existingCrName, Namespace: existingCrNamespace}
	}

	setupApiClient()
	strategy.Init()

	// Wait to finish deployment before running any tests
	waitUntilDeployed()
})

var _ = AfterSuite(func() {
	strategy.Cleanup()
})

func expectSuccessOrNotFound(err error) {
	if err != nil && !errors.IsNotFound(err) {
		Expect(err).ToNot(HaveOccurred())
	}
}

func setupApiClient() {
	Expect(sspv1beta1.AddToScheme(scheme.Scheme)).ToNot(HaveOccurred())
	Expect(promv1.AddToScheme(scheme.Scheme)).ToNot(HaveOccurred())
	Expect(templatev1.Install(scheme.Scheme)).ToNot(HaveOccurred())
	Expect(secv1.Install(scheme.Scheme)).ToNot(HaveOccurred())

	cfg, err := config.GetConfig()
	Expect(err).ToNot(HaveOccurred())
	apiClient, err = client.New(cfg, client.Options{})
	Expect(err).ToNot(HaveOccurred())

	ctx = context.Background()
	sspListerWatcher = createSspListerWatcher(cfg)
}

func createSspListerWatcher(cfg *rest.Config) cache.ListerWatcher {
	sspGvk, err := apiutil.GVKForObject(&sspv1beta1.SSP{}, scheme.Scheme)
	Expect(err).ToNot(HaveOccurred())

	restClient, err := apiutil.RESTClientForGVK(sspGvk, cfg, serializer.NewCodecFactory(scheme.Scheme))
	Expect(err).ToNot(HaveOccurred())

	return cache.NewListWatchFromClient(restClient, "ssps", strategy.GetNamespace(), fields.Everything())
}

func getSsp() *sspv1beta1.SSP {
	key := client.ObjectKey{Name: strategy.GetName(), Namespace: strategy.GetNamespace()}
	foundSsp := &sspv1beta1.SSP{}
	Expect(apiClient.Get(ctx, key, foundSsp)).ToNot(HaveOccurred())
	return foundSsp
}

func waitUntilDeployed() {
	if deploymentTimedOut {
		Fail("Timed out waiting for SSP to be in phase Deployed.")
	}

	// Set to true before waiting. In case Eventually fails,
	// it will panic and the deploymentTimedOut will be left true
	deploymentTimedOut = true
	EventuallyWithOffset(1, func() bool {
		return getSsp().Status.Phase == lifecycleapi.PhaseDeployed
	}, timeout, time.Second).Should(BeTrue())
	deploymentTimedOut = false
}

func waitForDeletion(key client.ObjectKey, obj runtime.Object) {
	EventuallyWithOffset(1, func() bool {
		err := apiClient.Get(ctx, key, obj)
		return errors.IsNotFound(err)
	}, timeout, time.Second).Should(BeTrue())
}

func getBoolEnv(envName string) bool {
	envVal := os.Getenv(envName)
	if envVal == "" {
		return false
	}
	val, err := strconv.ParseBool(envVal)
	if err != nil {
		return false
	}
	return val
}

func updateSsp(updateFunc func(foundSsp *sspv1beta1.SSP)) {
	Eventually(func() error {
		foundSsp := getSsp()
		updateFunc(foundSsp)
		return apiClient.Update(ctx, foundSsp)
	}, timeout, time.Second).ShouldNot(HaveOccurred())
}

func createOrUpdateSsp(ssp *sspv1beta1.SSP) {
	key := client.ObjectKey{
		Name:      ssp.Name,
		Namespace: ssp.Namespace,
	}
	Eventually(func() error {
		foundSsp := &sspv1beta1.SSP{}
		err := apiClient.Get(ctx, key, foundSsp)
		if err == nil {
			if reflect.DeepEqual(foundSsp.Spec, ssp.Spec) {
				return nil
			}
			foundSsp.Spec = ssp.Spec
			return apiClient.Update(ctx, foundSsp)
		}
		if errors.IsNotFound(err) {
			newSsp := &sspv1beta1.SSP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ssp.Name,
					Namespace: ssp.Namespace,
				},
				Spec: ssp.Spec,
			}
			return apiClient.Create(ctx, newSsp)
		}
		return err
	}, timeout, time.Second).ShouldNot(HaveOccurred())
}

func triggerReconciliation() {
	updateSsp(func(foundSsp *sspv1beta1.SSP) {
		if foundSsp.GetAnnotations() == nil {
			foundSsp.SetAnnotations(map[string]string{})
		}

		foundSsp.GetAnnotations()["forceReconciliation"] = ""
	})

	updateSsp(func(foundSsp *sspv1beta1.SSP) {
		if foundSsp.GetAnnotations() == nil {
			foundSsp.SetAnnotations(map[string]string{})
		}

		delete(foundSsp.GetAnnotations(), "forceReconciliation")
	})
}

func TestFunctional(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Functional test suite")
}
