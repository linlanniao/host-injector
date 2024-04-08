package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"

	jsonpatch "gomodules.xyz/jsonpatch/v2"
	v1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

const (
	watchingLabelKey = "k8s-app"
)

func isWatching(pod *corev1.Pod) bool {
	if _, ok := pod.Labels[watchingLabelKey]; !ok {
		return false
	} else {
		return true
	}
}

var (
	_cli       *kubernetes.Clientset
	initClient sync.Once
)

func newClient() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		var configPath string
		if p := os.Getenv(clientcmd.RecommendedConfigPathEnvVar); len(p) > 0 {
			configPath = p
		} else {
			configPath = clientcmd.RecommendedHomeFile
		}
		config, err = clientcmd.BuildConfigFromFlags("", configPath)
	}

	if err != nil {
		err = fmt.Errorf("error building kubeconfig: %w", err)
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

func client() *kubernetes.Clientset {
	initClient.Do(func() {
		var (
			err error
			c   *kubernetes.Clientset
		)
		defer func() {
			if err != nil {
				klog.Error(err.Error())
				panic(err.Error())
			}
		}()

		c, err = newClient()
		if err != nil {
			err = fmt.Errorf("error creating Kubernetes client: %w", err)
			slog.Error(err.Error())
			panic(err.Error())
		}
		_cli = c
	})

	return _cli
}

func getHostAliasesFromServices() ([]corev1.HostAlias, error) {
	services, err := client().CoreV1().Services("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	if len(services.Items) == 0 {
		return nil, nil
	}

	hostAliases := make([]corev1.HostAlias, 0)

	for _, service := range services.Items {
		if service.Spec.Type != corev1.ServiceTypeClusterIP {
			continue
		}
		if service.Spec.ClusterIP == "" || service.Spec.ClusterIP == "None" {
			continue
		}

		domain1 := fmt.Sprintf("%s.%s.svc.cluster.local", service.GetName(), service.GetNamespace())
		domain2 := fmt.Sprintf("%s.%s.svc", service.GetName(), service.GetNamespace())
		domain3 := fmt.Sprintf("%s.%s", service.GetName(), service.GetNamespace())
		hostAliases = append(hostAliases, corev1.HostAlias{
			IP:        service.Spec.ClusterIP,
			Hostnames: []string{domain1, domain2, domain3},
		})
	}

	return hostAliases, nil
}

func mutatePods(ctx context.Context, req *v1.AdmissionReview) *v1.AdmissionResponse {
	// Assuming the incoming request is of kind Pod
	pod := corev1.Pod{}
	if err := json.Unmarshal(req.Request.Object.Raw, &pod); err != nil {
		return &v1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	if !isWatching(&pod) {
		return &v1.AdmissionResponse{
			Allowed: true,
			Result: &metav1.Status{
				Message: "Pod is not watching",
			},
		}
	}

	hostAliases, err := getHostAliasesFromServices()
	if err != nil {
		return &v1.AdmissionResponse{Warnings: []string{
			fmt.Sprintf("Failed to get host aliases: %s", err.Error()),
		}}
	}

	if len(hostAliases) == 0 {
		return &v1.AdmissionResponse{
			Allowed: true,
			Result: &metav1.Status{
				Message: "No host aliases found",
			},
		}
	}

	//if pod.Spec.HostAliases == nil {
	//	pod.Spec.HostAliases = make([]corev1.HostAlias, 0, len(hostAliases))
	//}
	//for _, hostAlias := range hostAliases {
	//	pod.Spec.HostAliases = append(pod.Spec.HostAliases, hostAlias)
	//}

	patch, err := json.Marshal([]map[string]interface{}{
		{
			"op":    "add",
			"path":  "/metadata/labels/env",
			"value": "dev",
		},
	})
	if err != nil {
		return &v1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	return &v1.AdmissionResponse{
		Allowed: true,
		Patch:   patch,
		PatchType: func() *v1.PatchType {
			pt := v1.PatchTypeJSONPatch
			return &pt
		}(),
	}

	resp, err := json.Marshal(pod)
	if err != nil {
		return &v1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}
	patches, err := jsonpatch.CreatePatch(req.Request.Object.Raw, resp)
	if err != nil {
		return &v1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	patchBytes := make([]byte, 0)
	for _, p := range patches {
		b, _ := p.MarshalJSON()
		patchBytes = append(patchBytes, b...)
	}

	return &v1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *v1.PatchType {
			if len(patches) == 0 {
				return nil
			}
			pt := v1.PatchTypeJSONPatch
			return &pt
		}(),
	}
}

func handleMutatePod(w http.ResponseWriter, r *http.Request) {

	var admissionReview v1.AdmissionReview

	if err := json.NewDecoder(r.Body).Decode(&admissionReview); err != nil {
		http.Error(w, "could not decode request body", http.StatusBadRequest)
		return
	}
	slog.Info("hello1")
	admissionResponse := mutatePods(context.Background(), &admissionReview)
	slog.Info("hello2")
	admissionReview.Response = admissionResponse

	slog.Info("hello3")
	if err := json.NewEncoder(w).Encode(admissionReview); err != nil {
		http.Error(w, "could not encode response", http.StatusInternalServerError)
		return
	}
	return
}

func main() {
	http.HandleFunc("/mutate-core-v1-pod", handleMutatePod)
	_ = http.ListenAndServeTLS(":9443", "testcerts/tls.crt", "testcerts/tls.key", nil)
}
