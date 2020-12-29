package template_validator

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	admission "k8s.io/api/admissionregistration/v1"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/pointer"
	. "kubevirt.io/ssp-operator/internal/test-utils"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ssp "kubevirt.io/ssp-operator/api/v1beta1"
	"kubevirt.io/ssp-operator/internal/common"
)

var log = logf.Log.WithName("validator_operand")

var _ = Describe("Template validator operand", func() {
	const (
		namespace       = "kubevirt"
		name            = "test-ssp"
		replicas  int32 = 2
	)

	var (
		request common.Request
		operand = GetOperand()
	)

	BeforeEach(func() {
		s := scheme.Scheme
		Expect(ssp.AddToScheme(s)).ToNot(HaveOccurred())

		client := fake.NewFakeClientWithScheme(s)
		request = common.Request{
			Request: reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: namespace,
					Name:      name,
				},
			},
			Client:  client,
			Scheme:  s,
			Context: context.Background(),
			Instance: &ssp.SSP{
				TypeMeta: meta.TypeMeta{
					Kind:       "SSP",
					APIVersion: ssp.GroupVersion.String(),
				},
				ObjectMeta: meta.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Spec: ssp.SSPSpec{
					TemplateValidator: ssp.TemplateValidator{
						Replicas: pointer.Int32Ptr(replicas),
					},
				},
			},
			Logger:       log,
			VersionCache: common.VersionCache{},
		}
	})

	It("should create validator resources", func() {
		_, errs := operand.Reconcile(&request)
		ExpectNoErrors(errs)

		ExpectResourceExists(newClusterRole(namespace), request)
		ExpectResourceExists(newServiceAccount(namespace), request)
		ExpectResourceExists(newClusterRoleBinding(namespace), request)
		ExpectResourceExists(newService(namespace), request)
		ExpectResourceExists(newDeployment(namespace, replicas, "test-img"), request)
		ExpectResourceExists(newValidatingWebhook(namespace), request)
	})

	It("should not update webhook CA bundle", func() {
		_, errs := operand.Reconcile(&request)
		ExpectNoErrors(errs)

		key, err := client.ObjectKeyFromObject(newValidatingWebhook(namespace))
		Expect(err).ToNot(HaveOccurred())
		webhook := &admission.ValidatingWebhookConfiguration{}
		Expect(request.Client.Get(request.Context, key, webhook)).ToNot(HaveOccurred())

		const testCaBundle = "testCaBundle"
		webhook.Webhooks[0].ClientConfig.CABundle = []byte(testCaBundle)
		Expect(request.Client.Update(request.Context, webhook)).ToNot(HaveOccurred())

		_, errs = operand.Reconcile(&request)
		ExpectNoErrors(errs)

		updatedWebhook := &admission.ValidatingWebhookConfiguration{}
		Expect(request.Client.Get(request.Context, key, updatedWebhook)).ToNot(HaveOccurred())
		Expect(updatedWebhook.Webhooks[0].ClientConfig.CABundle).To(Equal([]byte(testCaBundle)))
	})

	It("should not update service cluster IP", func() {
		_, errs := operand.Reconcile(&request)
		ExpectNoErrors(errs)

		key, err := client.ObjectKeyFromObject(newService(namespace))
		Expect(err).ToNot(HaveOccurred())
		service := &core.Service{}
		Expect(request.Client.Get(request.Context, key, service)).ToNot(HaveOccurred())

		const testClusterIp = "1.2.3.4"
		service.Spec.ClusterIP = testClusterIp
		Expect(request.Client.Update(request.Context, service)).ToNot(HaveOccurred())

		_, errs = operand.Reconcile(&request)
		ExpectNoErrors(errs)

		updatedService := &core.Service{}
		Expect(request.Client.Get(request.Context, key, updatedService)).ToNot(HaveOccurred())
		Expect(updatedService.Spec.ClusterIP).To(Equal(testClusterIp))
	})

	It("should remove cluster resources on cleanup", func() {
		_, errs := operand.Reconcile(&request)
		ExpectNoErrors(errs)

		ExpectResourceExists(newClusterRole(namespace), request)
		ExpectResourceExists(newClusterRoleBinding(namespace), request)
		ExpectResourceExists(newValidatingWebhook(namespace), request)

		Expect(operand.Cleanup(&request)).ToNot(HaveOccurred())

		ExpectResourceNotExists(newClusterRole(namespace), request)
		ExpectResourceNotExists(newClusterRoleBinding(namespace), request)
		ExpectResourceNotExists(newValidatingWebhook(namespace), request)
	})

	It("should report status", func() {
		statuses, errs := operand.Reconcile(&request)
		ExpectNoErrors(errs)

		// Set status for deployment
		key, _ := client.ObjectKeyFromObject(newDeployment(namespace, replicas, "test-img"))
		updateDeployment(key, &request, func(deployment *apps.Deployment) {
			deployment.Status.Replicas = replicas
			deployment.Status.ReadyReplicas = 0
			deployment.Status.AvailableReplicas = 0
			deployment.Status.UpdatedReplicas = 0
			deployment.Status.UnavailableReplicas = replicas
		})

		statuses, errs = operand.Reconcile(&request)
		ExpectNoErrors(errs)

		// Only deployment should be progressing
		for _, status := range statuses {
			if _, ok := status.Resource.(*apps.Deployment); ok {
				Expect(status.NotAvailable).ToNot(BeNil())
				Expect(status.Progressing).ToNot(BeNil())
				Expect(status.Degraded).ToNot(BeNil())
			} else {
				Expect(status.NotAvailable).To(BeNil())
				Expect(status.Progressing).To(BeNil())
				Expect(status.Degraded).To(BeNil())
			}
		}

		updateDeployment(key, &request, func(deployment *apps.Deployment) {
			deployment.Status.Replicas = replicas
			deployment.Status.ReadyReplicas = replicas
			deployment.Status.AvailableReplicas = replicas
			deployment.Status.UpdatedReplicas = replicas
			deployment.Status.UnavailableReplicas = 0
		})

		statuses, errs = operand.Reconcile(&request)
		ExpectNoErrors(errs)

		// All resources should be available
		for _, status := range statuses {
			Expect(status.NotAvailable).To(BeNil())
			Expect(status.Progressing).To(BeNil())
			Expect(status.Degraded).To(BeNil())
		}
	})
})

func updateDeployment(key client.ObjectKey, request *common.Request, updateFunc func(deployment *apps.Deployment)) {
	deployment := &apps.Deployment{}
	Expect(request.Client.Get(request.Context, key, deployment)).ToNot(HaveOccurred())
	updateFunc(deployment)
	Expect(request.Client.Update(request.Context, deployment)).ToNot(HaveOccurred())
	Expect(request.Client.Status().Update(request.Context, deployment)).ToNot(HaveOccurred())
}

func TestValidator(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Template Validator Suite")
}
