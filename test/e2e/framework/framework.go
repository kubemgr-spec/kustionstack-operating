/*
Copyright 2023 The KusionStack Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package framework

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultNamespaceDeletionTimeout is timeout duration for waiting for a namespace deletion.
	DefaultNamespaceDeletionTimeout = 5 * time.Minute
	defaultServiceAccountName       = "default"
)

var (
	// NewFrameworkExtensions lists functions that get called by
	// NewFramework after constructing a new framework and after
	// calling ginkgo.BeforeEach for the framework.
	//
	// This can be used by extensions of the core framework to modify
	// settings in the framework instance or to add additional callbacks
	// with gingko.BeforeEach/AfterEach/DeferCleanup.
	//
	// When a test runs, functions will be invoked in this order:
	// - BeforeEaches defined by tests before f.NewDefaultFramework
	//   in the order in which they were defined (first-in-first-out)
	// - f.BeforeEach
	// - BeforeEaches defined by tests after f.NewDefaultFramework
	// - It callback
	// - all AfterEaches in the order in which they were defined
	// - all DeferCleanups with the order reversed (first-in-last-out)
	// - f.AfterEach
	//
	// Because a test might skip test execution in a BeforeEach that runs
	// before f.BeforeEach, AfterEach callbacks that depend on the
	// framework instance must check whether it was initialized. They can
	// do that by checking f.ClientSet for nil. DeferCleanup callbacks
	// don't need to do this because they get defined when the test
	// runs.
	NewFrameworkExtensions []func(f *Framework)
)

// Framework supports common operations used by e2e tests; it will keep a client & a namespace for you.
// Eventual goal is to merge this with integration test framework.
type Framework struct {
	BaseName string

	// Set together with creating the ClientSet and the namespace.
	// Guaranteed to be unique in the cluster even when running the same
	// test multiple times in parallel.
	UniqueName string

	Client    client.Client
	ClientSet clientset.Interface

	DynamicClient dynamic.Interface

	SkipNamespaceCreation    bool                // Whether to skip creating a namespace
	Namespace                *corev1.Namespace   // Every test has at least one namespace unless creation is skipped
	namespacesToDelete       []*corev1.Namespace // Some tests have more than one.
	NamespaceDeletionTimeout time.Duration
	SkipPrivilegedPSPBinding bool // Whether to skip creating a binding to the privileged PSP in the test namespace

	// To make sure that this framework cleans up after itself, no matter what,
	// we install a Cleanup action before each test and clear it after.  If we
	// should abort, the AfterSuite hook should run all Cleanup actions.
	cleanupHandle CleanupActionHandle

	// configuration for framework's client
	Options Options

	// Place where various additional data is stored during test run to be printed to ReportDir,
	// or stdout if ReportDir is not set once test ends.
	TestSummaries []TestDataSummary

	AfterEachActions []func()
}

// Options is a struct for managing test framework options.
type Options struct {
	ClientQPS    float32
	ClientBurst  int
	GroupVersion *schema.GroupVersion
}

type TestDataSummary interface {
	SummaryKind() string
	PrintHumanReadable() string
	PrintJSON() string
}

// DumpAllNamespaceInfoAction is called after each failed test for namespaces
// created for the test.
type DumpAllNamespaceInfoAction func(ctx context.Context, f *Framework, namespace string)

// KusionstackDescribe is a wrapper for ginkgo.Describe.
func KusionstackDescribe(text string, body func()) bool {
	return ginkgo.Describe("[kusionstack] "+text, body)
}

// NewDefaultFramework makes a new framework and sets up a BeforeEach/AfterEach for
// you (you can write additional before/after each functions).
func NewDefaultFramework(baseName string) *Framework {
	options := Options{
		ClientQPS:   20,
		ClientBurst: 50,
	}
	return NewFramework(baseName, options, nil)
}

// NewFramework makes a new framework and sets up a BeforeEach/AfterEach
func NewFramework(baseName string, options Options, client clientset.Interface) *Framework {
	f := &Framework{
		BaseName:  baseName,
		Options:   options,
		ClientSet: client,
	}

	ginkgo.BeforeEach(f.BeforeEach)
	ginkgo.AfterEach(f.AfterEach)

	return f
}

// BeforeEach gets a client and makes a namespace.
func (f *Framework) BeforeEach() {
	// The fact that we need this feels like a bug in ginkgo.
	// https://github.com/onsi/ginkgo/issues/222
	f.cleanupHandle = AddCleanupAction(f.AfterEach)
	if f.ClientSet == nil {
		ginkgo.By("Creating a kubernetes client")
		config, err := LoadConfig()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		testDesc := ginkgo.CurrentGinkgoTestDescription()
		if len(testDesc.ComponentTexts) > 0 {
			componentTexts := strings.Join(testDesc.ComponentTexts, " ")
			config.UserAgent = fmt.Sprintf(
				"%v -- %v",
				rest.DefaultKubernetesUserAgent(),
				componentTexts)
		}

		config.QPS = f.Options.ClientQPS
		config.Burst = f.Options.ClientBurst
		if f.Options.GroupVersion != nil {
			config.GroupVersion = f.Options.GroupVersion
		}
		if TestContext.KubeAPIContentType != "" {
			config.ContentType = TestContext.KubeAPIContentType
		}
		f.ClientSet, err = clientset.NewForConfig(config)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		f.DynamicClient, err = dynamic.NewForConfig(config)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// csi.storage.k8s.io is based on CRD, which is served only as JSON
		jsonConfig := config
		jsonConfig.ContentType = "application/json"
		f.Client, err = client.New(config, client.Options{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// create scales getter, set GroupVersion and NegotiatedSerializer to default values
		// as they are required when creating a REST client.
		if config.GroupVersion == nil {
			config.GroupVersion = &schema.GroupVersion{}
		}
		TestContext.CloudConfig.Provider.FrameworkBeforeEach(f)
	}

	if !f.SkipNamespaceCreation {
		ginkgo.By(fmt.Sprintf("Building a namespace api object, basename %s", f.BaseName))
		namespace, err := f.CreateNamespace(f.BaseName, map[string]string{
			"e2e-framework": f.BaseName,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		f.Namespace = namespace

		if TestContext.VerifyServiceAccount {
			ginkgo.By("Waiting for a default service account to be provisioned in namespace " + namespace.Name)
			err = WaitForDefaultServiceAccountInNamespace(f.ClientSet, namespace.Name)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		} else {
			Logf("Skipping waiting for service account")
		}
		f.UniqueName = f.Namespace.GetName()
	} else {
		// not guaranteed to be unique, but very likely
		f.UniqueName = fmt.Sprintf("%s-%08x", f.BaseName, rand.Int31())
	}
}

// AfterEach deletes the namespace, after reading its events.
func (f *Framework) AfterEach() {
	RemoveCleanupAction(f.cleanupHandle)

	// DeleteNamespace at the very end in defer, to avoid any
	// expectation failures preventing deleting the namespace.
	defer func() {
		nsDeletionErrors := map[string]error{}
		// Whether to delete namespace is determined by 3 factors: delete-namespace flag, delete-namespace-on-failure flag and the test result
		// if delete-namespace set to false, namespace will always be preserved.
		// if delete-namespace is true and delete-namespace-on-failure is false, namespace will be preserved if test failed.
		if TestContext.DeleteNamespace && (TestContext.DeleteNamespaceOnFailure || !ginkgo.CurrentGinkgoTestDescription().Failed) {
			for _, ns := range f.namespacesToDelete {
				ginkgo.By(fmt.Sprintf("Destroying namespace %q for this suite.", ns.Name))
				timeout := DefaultNamespaceDeletionTimeout
				if f.NamespaceDeletionTimeout != 0 {
					timeout = f.NamespaceDeletionTimeout
				}
				if err := deleteNS(f.ClientSet, f.DynamicClient, ns.Name, timeout); err != nil {
					if !apierrors.IsNotFound(err) {
						nsDeletionErrors[ns.Name] = err
					} else {
						Logf("Namespace %v was already deleted", ns.Name)
					}
				}
			}
		} else {
			if !TestContext.DeleteNamespace {
				Logf("Found DeleteNamespace=false, skipping namespace deletion!")
			} else {
				Logf("Found DeleteNamespaceOnFailure=false and current test failed, skipping namespace deletion!")
			}
		}

		// Paranoia-- prevent reuse!
		f.Namespace = nil
		f.Client = nil
		f.ClientSet = nil
		f.namespacesToDelete = nil

		// if we had errors deleting, report them now.
		if len(nsDeletionErrors) != 0 {
			messages := []string{}
			for namespaceKey, namespaceErr := range nsDeletionErrors {
				messages = append(messages, fmt.Sprintf("Couldn't delete ns: %q: %s (%#v)", namespaceKey, namespaceErr, namespaceErr))
			}
			Failf(strings.Join(messages, ","))
		}
	}()

	// Print events if the test failed.
	if ginkgo.CurrentGinkgoTestDescription().Failed && TestContext.DumpLogsOnFailure {
		// Pass both unversioned client and versioned clientset, till we have removed all uses of the unversioned client.
		if !f.SkipNamespaceCreation {
			DumpAllNamespaceInfo(f.ClientSet, f.Namespace.Name)
		}
	}

	for _, f := range f.AfterEachActions {
		f()
	}

	TestContext.CloudConfig.Provider.FrameworkAfterEach(f)

	// Check whether all nodes are ready after the test.
	// This is explicitly done at the very end of the test, to avoid
	// e.g. not removing namespace in case of this failure.
	//if err := AllNodesReady(f.ClientSet, 3*time.Minute); err != nil {
	//	Failf("All nodes should be ready after test, %v", err)
	//}
}

// CreateNamespace is used to create namespace
func (f *Framework) CreateNamespace(baseName string, labels map[string]string) (*corev1.Namespace, error) {
	createTestingNS := TestContext.CreateTestingNS
	if createTestingNS == nil {
		createTestingNS = CreateTestingNS
	}
	ns, err := createTestingNS(baseName, f.ClientSet, labels)
	// check ns instead of err to see if it's nil as we may
	// fail to create serviceAccount in it.
	f.AddNamespacesToDelete(ns)

	return ns, err
}

// AddNamespacesToDelete adds one or more namespaces to be deleted when the test
// completes.
func (f *Framework) AddNamespacesToDelete(namespaces ...*corev1.Namespace) {
	for _, ns := range namespaces {
		if ns == nil {
			continue
		}
		f.namespacesToDelete = append(f.namespacesToDelete, ns)

	}
}

// WaitForPodRunning waits for the pod to run in the namespace.
func (f *Framework) WaitForPodRunning(podName string) error {
	return WaitForPodNameRunningInNamespace(f.ClientSet, podName, f.Namespace.Name)
}

// KoribtoDescribe is a wrapper function for ginkgo describe.
func KoribtoDescribe(text string, body func()) bool {
	return ginkgo.Describe("[koribto.io] "+text, body)
}

// ConformanceIt is a wrapper function for ginkgo It.  Adds "[Conformance]" tag and makes static analysis easier.
func ConformanceIt(text string, body interface{}, timeout ...float64) bool {
	return ginkgo.It(text+" [Conformance]", body, timeout...)
}
