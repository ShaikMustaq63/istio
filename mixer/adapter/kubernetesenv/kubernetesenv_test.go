// Copyright 2017 Istio Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubernetesenv

import (
	"context"
	"errors"
	"net"
	"os"
	"reflect"
	"testing"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"istio.io/istio/mixer/adapter/kubernetesenv/config"
	kubernetes_apa_tmpl "istio.io/istio/mixer/adapter/kubernetesenv/template"
	"istio.io/istio/mixer/pkg/adapter"
	"istio.io/istio/mixer/pkg/adapter/test"
)

type fakeK8sBuilder struct {
	calledPath string
	calledEnv  adapter.Env
}

func (b *fakeK8sBuilder) build(path string, env adapter.Env) (kubernetes.Interface, error) {
	b.calledPath = path
	b.calledEnv = env
	return fake.NewSimpleClientset(), nil
}

func errorClientBuilder(path string, env adapter.Env) (kubernetes.Interface, error) {
	return nil, errors.New("can't build k8s client")
}

// note: not using TestAdapterInvariants here because of kubernetes dependency.
// we are aiming for simple unit testing. a larger, more involved integration
// test / e2e test must be written to validate the builder in relation to a
// real kubernetes cluster.
func TestBuilder(t *testing.T) {
	b := newBuilder((&fakeK8sBuilder{}).build)

	if err := b.Validate(); err != nil {
		t.Errorf("ValidateConfig() => builder can't validate its default configuration: %v", err)
	}
}

func TestBuilder_ValidateConfigErrors(t *testing.T) {
	tests := []struct {
		name     string
		conf     *config.Params
		errCount int
	}{
		{"empty config", &config.Params{}, 4},
		{"bad cluster domain name", &config.Params{ClusterDomainName: "something.silly", PodLabelForService: "app"}, 3},
	}

	b := newBuilder((&fakeK8sBuilder{}).build)

	for _, v := range tests {
		b.SetAdapterConfig(v.conf)
		err := b.Validate()
		if err == nil {
			t.Fatalf("Expected config to fail validation: %#v", v.conf)
		}

		if len(err.Multi.Errors) != v.errCount {
			t.Fatalf("Got %d errors; wanted %d", len(err.Multi.Errors), v.errCount)
		}
	}
}

func TestBuilder_BuildAttributesGenerator(t *testing.T) {
	tests := []struct {
		name    string
		testFn  clientFactoryFn
		conf    adapter.Config
		wantErr bool
	}{
		{"success", (&fakeK8sBuilder{}).build, conf, false},
		{"builder error", errorClientBuilder, conf, true},
	}

	for _, v := range tests {
		t.Run(v.name, func(t *testing.T) {
			b := newBuilder(v.testFn)
			b.SetAdapterConfig(v.conf)
			_, err := b.Build(context.Background(), test.NewEnv(t))
			if err == nil && v.wantErr {
				t.Fatal("Expected error building adapter")
			}
			if err != nil && !v.wantErr {
				t.Fatalf("Got error, wanted none: %v", err)
			}
		})
	}
}

func TestBuilder_BuildAttributesGeneratorWithEnvVar(t *testing.T) {
	testConf := *conf
	testConf.KubeconfigPath = "please/override"

	tests := []struct {
		name          string
		clientFactory *fakeK8sBuilder
		conf          adapter.Config
		wantOK        bool
	}{
		{"success", &fakeK8sBuilder{}, &testConf, true},
	}

	wantPath := "/want/kubeconfig"
	if err := os.Setenv("KUBECONFIG", wantPath); err != nil {
		t.Fatalf("Could not set KUBECONFIG environment var")
	}

	for _, v := range tests {
		t.Run(v.name, func(t *testing.T) {
			b := newBuilder(v.clientFactory.build)
			b.SetAdapterConfig(v.conf)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			{
				_, err := b.Build(ctx, test.NewEnv(t))
				gotOK := err == nil
				if gotOK != v.wantOK {
					t.Fatalf("Got %v, Want %v", err, v.wantOK)
				}
				if v.clientFactory.calledPath != wantPath {
					t.Errorf("Bad kubeconfig path; got %s, want %s", v.clientFactory.calledPath, wantPath)
				}
			}
			v.clientFactory.calledPath = ""

			// try this another time. create a new handler from the same builder
			{
				_, err := b.Build(ctx, test.NewEnv(t))
				gotOK := err == nil
				if gotOK != v.wantOK {
					t.Fatalf("Got %v, Want %v", err, v.wantOK)
				}
				if v.clientFactory.calledPath != wantPath {
					t.Errorf("Bad kubeconfig path; got %s, want %s", v.clientFactory.calledPath, wantPath)
				}
			}
		})
	}
}

func TestKubegen_Generate(t *testing.T) {
	pods := map[string]*v1.Pod{
		"testns/test-pod": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "testns",
				Labels: map[string]string{
					"app":       "test",
					"something": "",
				},
			},
			Status: v1.PodStatus{
				HostIP: "10.1.1.10",
				PodIP:  "10.10.10.1",
			},
			Spec: v1.PodSpec{
				ServiceAccountName: "test",
			},
		},
		"testns/pod-cluster": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-cluster",
				Namespace: "testns",
				Labels:    map[string]string{"app": "alt-svc-with-cluster.testns.svc.cluster:8080"},
			},
		},
		"testns/long-pod": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "long-pod",
				Namespace: "testns",
				Labels: map[string]string{
					"app": "long-svc.testns.svc.cluster.local.solar",
				},
			},
		},
		"testns/empty":       {ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "testns", Labels: map[string]string{"app": ""}}},
		"testns/alt-pod":     {ObjectMeta: metav1.ObjectMeta{Name: "alt-pod", Namespace: "testns", Labels: map[string]string{"app": "alt-svc.testns"}}},
		"testns/bad-svc-pod": {ObjectMeta: metav1.ObjectMeta{Name: "bad-svc-pod", Namespace: "testns", Labels: map[string]string{"app": ":"}}},
		"192.168.234.3": {
			ObjectMeta: metav1.ObjectMeta{Name: "ip-svc-pod", Namespace: "testns", Labels: map[string]string{"app": "ipAddr"}},
			Status:     v1.PodStatus{PodIP: "192.168.234.3"},
		},
		"istio-system/ingress": {ObjectMeta: metav1.ObjectMeta{Name: "ingress", Namespace: "istio-system", Labels: map[string]string{"istio": "ingress"}}},
		"testns/ipApp":         {ObjectMeta: metav1.ObjectMeta{Name: "ipApp", Namespace: "testns", Labels: map[string]string{"app": "10.1.10.1"}}},
	}

	sourceUIDIn := &kubernetes_apa_tmpl.Instance{
		SourceUid:      "kubernetes://test-pod.testns",
		DestinationUid: "kubernetes://badsvcuid",
		OriginUid:      "kubernetes://badsvcuid",
	}

	sourceUIDOut := &kubernetes_apa_tmpl.Output{
		SourceLabels: map[string]string{
			"app":       "test",
			"something": "",
		},
		SourcePodIp:              net.ParseIP("10.10.10.1"),
		SourceHostIp:             net.ParseIP("10.1.1.10"),
		SourceNamespace:          "testns",
		SourcePodName:            "test-pod",
		SourceService:            "test.testns.svc.cluster.local",
		SourceServiceAccountName: "test",
	}

	nsAppLabelIn := &kubernetes_apa_tmpl.Instance{
		SourceUid: "kubernetes://alt-pod.testns",
	}

	nsAppLabelOut := &kubernetes_apa_tmpl.Output{
		SourceLabels: map[string]string{
			"app": "alt-svc.testns",
		},
		SourceService:   "alt-svc.testns.svc.cluster.local",
		SourceNamespace: "testns",
		SourcePodName:   "alt-pod",
	}

	svcClusterIn := &kubernetes_apa_tmpl.Instance{SourceUid: "kubernetes://pod-cluster.testns"}

	svcClusterOut := &kubernetes_apa_tmpl.Output{
		SourceLabels: map[string]string{
			"app": "alt-svc-with-cluster.testns.svc.cluster:8080",
		},
		SourceService:   "alt-svc-with-cluster.testns.svc.cluster.local",
		SourceNamespace: "testns",
		SourcePodName:   "pod-cluster",
	}

	longSvcClusterIn := &kubernetes_apa_tmpl.Instance{SourceUid: "kubernetes://long-pod.testns"}

	longSvcClusterOut := &kubernetes_apa_tmpl.Output{
		SourceLabels: map[string]string{
			"app": "long-svc.testns.svc.cluster.local.solar",
		},
		SourceService:   "long-svc.testns.svc.cluster.local.solar",
		SourceNamespace: "testns",
		SourcePodName:   "long-pod",
	}

	emptySvcIn := &kubernetes_apa_tmpl.Instance{DestinationUid: "kubernetes://empty.testns"}

	emptyServiceOut := &kubernetes_apa_tmpl.Output{
		DestinationLabels: map[string]string{
			"app": "",
		},
		DestinationNamespace: "testns",
		DestinationPodName:   "empty",
	}

	badDestinationSvcIn := &kubernetes_apa_tmpl.Instance{DestinationUid: "kubernetes://bad-svc-pod.testns"}

	badDestinationOut := &kubernetes_apa_tmpl.Output{
		DestinationLabels: map[string]string{
			"app": ":",
		},
		DestinationNamespace: "testns",
		DestinationPodName:   "bad-svc-pod",
	}

	ipDestinationSvcIn := &kubernetes_apa_tmpl.Instance{DestinationIp: net.ParseIP("192.168.234.3")}

	ipDestinationOut := &kubernetes_apa_tmpl.Output{
		DestinationLabels: map[string]string{
			"app": "ipAddr",
		},
		DestinationNamespace: "testns",
		DestinationPodName:   "ip-svc-pod",
		DestinationService:   "ipAddr.testns.svc.cluster.local",
		DestinationPodIp:     net.ParseIP("192.168.234.3"),
	}

	istioDestinationSvcIn := &kubernetes_apa_tmpl.Instance{
		DestinationUid: "kubernetes://ingress.istio-system",
		SourceUid:      "kubernetes://test-pod.testns",
	}

	istioDestinationOut := &kubernetes_apa_tmpl.Output{
		DestinationLabels: map[string]string{
			"istio": "ingress",
		},
		DestinationNamespace: "istio-system",
		DestinationPodName:   "ingress",
		DestinationService:   "ingress.istio-system.svc.cluster.local",
	}

	istioDestinationWithSrcOut := &kubernetes_apa_tmpl.Output{
		DestinationLabels:        map[string]string{"istio": "ingress"},
		DestinationNamespace:     "istio-system",
		DestinationPodName:       "ingress",
		DestinationService:       "ingress.istio-system.svc.cluster.local",
		SourceServiceAccountName: "test",
		SourceService:            "test.testns.svc.cluster.local",
		SourceLabels:             map[string]string{"app": "test", "something": ""},
		SourceNamespace:          "testns",
		SourcePodIp:              net.ParseIP("10.10.10.1"),
		SourceHostIp:             net.ParseIP("10.1.1.10"),
		SourcePodName:            "test-pod",
	}

	ipAppSvcIn := &kubernetes_apa_tmpl.Instance{
		DestinationUid: "kubernetes://ipApp.testns",
	}

	ipAppDestinationOut := &kubernetes_apa_tmpl.Output{
		DestinationLabels: map[string]string{
			"app": "10.1.10.1",
		},
		DestinationNamespace: "testns",
		DestinationPodName:   "ipApp",
	}

	confWithIngressLookups := *conf
	confWithIngressLookups.LookupIngressSourceAndOriginValues = true

	tests := []struct {
		name   string
		inputs *kubernetes_apa_tmpl.Instance
		want   *kubernetes_apa_tmpl.Output
		params *config.Params
	}{
		{"source pod and destination service", sourceUIDIn, sourceUIDOut, conf},
		{"alternate service canonicalization (namespace)", nsAppLabelIn, nsAppLabelOut, conf},
		{"alternate service canonicalization (svc cluster)", svcClusterIn, svcClusterOut, conf},
		{"alternate service canonicalization (long svc)", longSvcClusterIn, longSvcClusterOut, conf},
		{"empty service", emptySvcIn, emptyServiceOut, conf},
		{"bad destination service", badDestinationSvcIn, badDestinationOut, conf},
		{"destination ip pod", ipDestinationSvcIn, ipDestinationOut, conf},
		{"istio ingress service (no lookup source)", istioDestinationSvcIn, istioDestinationOut, conf},
		{"istio ingress service (lookup source)", istioDestinationSvcIn, istioDestinationWithSrcOut, &confWithIngressLookups},
		{"ip app", ipAppSvcIn, ipAppDestinationOut, conf},
	}

	objs := make([]runtime.Object, 0, len(pods))
	for _, pod := range pods {
		objs = append(objs, pod)
	}

	builder := newBuilder(func(string, adapter.Env) (kubernetes.Interface, error) {
		return fake.NewSimpleClientset(objs...), nil
	})

	for _, v := range tests {
		t.Run(v.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			builder.SetAdapterConfig(v.params)

			kg, err := builder.Build(ctx, test.NewEnv(t))
			if err != nil {
				t.Fatal(err)
			}

			got, err := kg.(*handler).GenerateKubernetesAttributes(ctx, v.inputs)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, v.want) {
				t.Errorf("Generate(): got %#v; want %#v", got, v.want)
			}
		})
	}
}
