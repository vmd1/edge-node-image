package config

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Kube is a minimal Kubernetes API client that authenticates as the node (the kubelet's client certificate that k3s writes
// under /var/lib/rancher/k3s/agent). It reads EdgeNodeConfig objects and this node's labels, and writes only annotations
// on its own Node with its own field manager. No client-go: noded has a 24 MiB budget on a 1 GB node.
type Kube struct {
	Base     string // https://host:6443
	NodeName string
	HTTP     *http.Client
}

const (
	AnnState = "node.vmd1.dev/config-state"
	AnnHash  = "node.vmd1.dev/config-hash"
	AnnError = "node.vmd1.dev/config-error"
	manager  = "edge-node-config"
)

// NewKube builds a client from the files k3s-agent maintains.
func NewKube(base, nodeName, agentDir string) (*Kube, error) {
	cert, err := tls.LoadX509KeyPair(agentDir+"/client-kubelet.crt", agentDir+"/client-kubelet.key")
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(agentDir + "/server-ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &Kube{Base: strings.TrimRight(base, "/"), NodeName: nodeName, HTTP: &http.Client{Timeout: 20 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS12}}}}, nil
}

func (k *Kube) do(ctx context.Context, method, path, ctype string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, k.Base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := k.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %.200s", method, path, resp.Status, b)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

type nodeDoc struct {
	Metadata struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Status struct {
		Conditions []struct{ Type, Status string } `json:"conditions"`
	} `json:"status"`
}

// Node returns this node's labels, annotations and whether it is Ready.
func (k *Kube) Node(ctx context.Context) (labels, ann map[string]string, ready bool, err error) {
	var n nodeDoc
	if err = k.do(ctx, "GET", "/api/v1/nodes/"+url.PathEscape(k.NodeName), "", nil, &n); err != nil {
		return
	}
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			ready = true
		}
	}
	return n.Metadata.Labels, n.Metadata.Annotations, ready, nil
}

// Configs lists the EdgeNodeConfig objects.
func (k *Kube) Configs(ctx context.Context) ([]Object, error) {
	var l struct {
		Items []struct {
			Metadata struct {
				Name       string `json:"name"`
				Generation int64  `json:"generation"`
			} `json:"metadata"`
			Spec Spec `json:"spec"`
		} `json:"items"`
	}
	if err := k.do(ctx, "GET", "/apis/edge.vmd1.dev/v1alpha1/edgenodeconfigs", "", nil, &l); err != nil {
		return nil, err
	}
	out := make([]Object, 0, len(l.Items))
	for _, i := range l.Items {
		out = append(out, Object{Name: i.Metadata.Name, Generation: i.Metadata.Generation, Spec: i.Spec})
	}
	return out, nil
}

// OthersApplying reports whether another node is mid-apply (annotation state "applying"), so rollouts stay one node at a time.
func (k *Kube) OthersApplying(ctx context.Context) (bool, error) {
	var l struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := k.do(ctx, "GET", "/api/v1/nodes?limit=200", "", nil, &l); err != nil {
		return false, err
	}
	for _, i := range l.Items {
		if i.Metadata.Name != k.NodeName && i.Metadata.Annotations[AnnState] == "applying" {
			return true, nil
		}
	}
	return false, nil
}

// Report writes the state annotations with this agent's own field manager.
func (k *Kube) Report(ctx context.Context, state, hash, errMsg string) error {
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{AnnState: state, AnnHash: hash, AnnError: errMsg}}})
	return k.do(ctx, "PATCH", "/api/v1/nodes/"+url.PathEscape(k.NodeName)+"?fieldManager="+manager, "application/merge-patch+json", patch, nil)
}
