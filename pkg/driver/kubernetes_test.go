package driver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
)

func TestInClusterPVCClientModes(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	for _, mode := range []string{"auto", "disabled"} {
		client, err := NewInClusterPVCClient(mode)
		if client != nil || err != nil {
			t.Fatalf("%s without cluster = %v, %v", mode, client, err)
		}
	}
	for _, mode := range []string{"enabled", "bad"} {
		if _, err := NewInClusterPVCClient(mode); err == nil {
			t.Fatalf("%s without cluster must fail", mode)
		}
	}
}

func kubernetesTestServer(t *testing.T) (*httptest.Server, *corev1.Event) {
	t.Helper()
	event := &corev1.Event{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/ns/persistentvolumeclaims/claim":
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"claim","namespace":"ns","uid":"uid"}}`))
		case "/api/v1/persistentvolumes/pv":
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PersistentVolume","metadata":{"name":"pv"}}`))
		case "/api/v1/persistentvolumes":
			if r.URL.Query().Get("continue") == "" {
				_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PersistentVolumeList","metadata":{"continue":"next"},"items":[{"metadata":{"name":"pv"}}]}`))
			} else {
				_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PersistentVolumeList","items":[{"metadata":{"name":"pv2"}}]}`))
			}
		case "/apis/storage.k8s.io/v1/storageclasses/btrfs":
			_, _ = w.Write([]byte(`{"apiVersion":"storage.k8s.io/v1","kind":"StorageClass","metadata":{"name":"btrfs"},"provisioner":"btrfs.csi.local"}`))
		case "/api/v1/namespaces/ns/events":
			if r.Method != http.MethodPost {
				t.Error("warning must create an Event")
			}
			if err := json.NewDecoder(r.Body).Decode(event); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(event)
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	return server, event
}

func TestKubernetesPVCClient(t *testing.T) {
	server, event := kubernetesTestServer(t)
	defer server.Close()
	client, err := newKubernetesPVCClient(&rest.Config{
		Host:          server.URL,
		ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pvc, err := client.GetPVC(t.Context(), "ns", "claim")
	if err != nil || pvc.Name != "claim" {
		t.Fatalf("GetPVC = %v, %v", pvc, err)
	}
	pv, err := client.GetPV(t.Context(), "pv")
	if err != nil || pv.Name != "pv" {
		t.Fatalf("GetPV = %v, %v", pv, err)
	}
	pvs, err := client.ListPVs(t.Context())
	if err != nil || len(pvs) != 2 {
		t.Fatalf("paginated ListPVs = %v, %v", pvs, err)
	}
	class, err := client.GetStorageClass(t.Context(), "btrfs")
	if err != nil || class.Provisioner != driverName {
		t.Fatalf("GetStorageClass = %v, %v", class, err)
	}
	if err := client.Warning(t.Context(), pvc, "bad compression"); err != nil {
		t.Fatal(err)
	}
	if event.Type != corev1.EventTypeWarning || event.InvolvedObject.UID != "uid" ||
		!strings.Contains(event.Message, "bad compression") {
		t.Fatalf("incorrect warning: %+v", event)
	}
}
