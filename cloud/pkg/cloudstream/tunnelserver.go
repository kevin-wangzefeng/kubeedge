/*
Copyright 2020 The KubeEdge Authors.

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

package cloudstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/emicklei/go-restful"
	"github.com/gorilla/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/klog/v2"

	hubconfig "github.com/kubeedge/kubeedge/cloud/pkg/cloudhub/config"
	streamconfig "github.com/kubeedge/kubeedge/cloud/pkg/cloudstream/config"
	"github.com/kubeedge/kubeedge/cloud/pkg/common/client"
	"github.com/kubeedge/kubeedge/pkg/stream"
)

const (
	// The amount of time the tunnelserver should sleep between retrying node status updates
	DefaultRetrySleepTime          = 20 * time.Second
	DefaultNodeStatusUpdateTimeout = 2 * time.Minute
)

type TunnelServer struct {
	container *restful.Container
	upgrader  websocket.Upgrader
	sync.Mutex
	sessions      map[string]*Session
	nodeNameIP    sync.Map
	tunnelPort    int
	kubeClient    v1.CoreV1Interface
	retrySleep    time.Duration
	updateTimeout time.Duration
}

func newTunnelServer(tunnelPort int) *TunnelServer {
	var kubeClient v1.CoreV1Interface
	// Safely get the kube client, handling the nil case for tests
	k8sClient := client.GetKubeClient()
	if k8sClient != nil {
		kubeClient = k8sClient.CoreV1()
	}

	return newTunnelServerWithClient(tunnelPort, kubeClient, DefaultRetrySleepTime, DefaultNodeStatusUpdateTimeout)
}

func newTunnelServerWithClient(tunnelPort int, kubeClient v1.CoreV1Interface, retrySleep, updateTimeout time.Duration) *TunnelServer {
	return &TunnelServer{
		container:     restful.NewContainer(),
		sessions:      make(map[string]*Session),
		tunnelPort:    tunnelPort,
		kubeClient:    kubeClient,
		retrySleep:    retrySleep,
		updateTimeout: updateTimeout,
		upgrader: websocket.Upgrader{
			HandshakeTimeout: time.Second * 2,
			ReadBufferSize:   1024,
			Error: func(w http.ResponseWriter, r *http.Request, status int, reason error) {
				w.WriteHeader(status)
				_, err := w.Write([]byte(reason.Error()))
				if err != nil {
					klog.Errorf("failed to write http response, err: %v", err)
				}
			},
		},
	}
}

func (s *TunnelServer) installDefaultHandler() {
	ws := new(restful.WebService)
	ws.Path("/v1/kubeedge/connect")
	ws.Route(ws.GET("/").
		To(s.connect))
	s.container.Add(ws)
}

func (s *TunnelServer) addSession(key string, session *Session) {
	s.Lock()
	s.sessions[key] = session
	s.Unlock()
}

func (s *TunnelServer) getSession(id string) (*Session, bool) {
	s.Lock()
	defer s.Unlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

func (s *TunnelServer) addNodeIP(node, ip string) {
	s.nodeNameIP.Store(node, ip)
}

func (s *TunnelServer) getNodeIP(node string) (string, bool) {
	ip, ok := s.nodeNameIP.Load(node)
	if !ok {
		return "", ok
	}
	return ip.(string), ok
}

func (s *TunnelServer) connect(r *restful.Request, w *restful.Response) {
	hostNameOverride := r.HeaderParameter(stream.SessionKeyHostNameOverride)
	internalIP := r.HeaderParameter(stream.SessionKeyInternalIP)
	if internalIP == "" {
		internalIP = strings.Split(r.Request.RemoteAddr, ":")[0]
	}
	con, err := s.upgrader.Upgrade(w, r.Request, nil)
	if err != nil {
		klog.Errorf("Failed to upgrade the HTTP server connection to the WebSocket protocol: %v", err)
		return
	}
	klog.Infof("get a new tunnel agent hostname %v, internalIP %v", hostNameOverride, internalIP)

	session := &Session{
		tunnel:        stream.NewDefaultTunnel(con),
		apiServerConn: make(map[uint64]APIServerConnection),
		apiConnlock:   &sync.RWMutex{},
		sessionID:     hostNameOverride,
	}

	err = s.updateNodeKubeletEndpoint(hostNameOverride)
	if err != nil {
		msg := stream.NewMessage(0, stream.MessageTypeCloseConnect, []byte(err.Error()))
		if err := session.tunnel.WriteMessage(msg); err == nil {
			klog.V(4).Infof("CloudStream send close connection message to edge successfully")
		} else {
			klog.Errorf("CloudStream failed to send close connection message to edge, error: %v", err)
		}
		return
	}
	s.addSession(hostNameOverride, session)
	s.addSession(internalIP, session)
	s.addNodeIP(hostNameOverride, internalIP)
	session.Serve()
}

func (s *TunnelServer) Start() {
	s.installDefaultHandler()
	var data []byte
	var key []byte
	var cert []byte

	if streamconfig.Config.Ca != nil {
		data = streamconfig.Config.Ca
		klog.Info("Succeed in loading TunnelCA from local directory")
	} else {
		data = hubconfig.Config.Ca
		klog.Info("Succeed in loading TunnelCA from CloudHub")
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: certutil.CertificateBlockType, Bytes: data}))

	if streamconfig.Config.Key != nil && streamconfig.Config.Cert != nil {
		cert = streamconfig.Config.Cert
		key = streamconfig.Config.Key
		klog.Info("Succeed in loading TunnelCert and Key from local directory")
	} else {
		cert = hubconfig.Config.Cert
		key = hubconfig.Config.Key
		klog.Info("Succeed in loading TunnelCert and Key from CloudHub")
	}

	certificate, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: certutil.CertificateBlockType, Bytes: cert}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	if err != nil {
		klog.Error("Failed to load TLSTunnelCert and Key")
		panic(err)
	}

	tunnelServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", streamconfig.Config.TunnelPort),
		Handler: s.container,
		TLSConfig: &tls.Config{
			ClientCAs:    pool,
			Certificates: []tls.Certificate{certificate},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   tls.VersionTLS12,
			CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		},
	}
	klog.Infof("Prepare to start tunnel server ...")
	err = tunnelServer.ListenAndServeTLS("", "")
	if err != nil {
		klog.Exitf("Start tunnelServer error %v\n", err)
		return
	}
}

func (s *TunnelServer) updateNodeKubeletEndpoint(nodeName string) error {
	if s.kubeClient == nil {
		klog.V(4).Info("Skip updating node kubelet endpoint in test mode")
		return fmt.Errorf("kubeclient is nil, cannot update node kubelet endpoint")
	}
	if err := wait.PollImmediate(s.retrySleep, s.updateTimeout, func() (bool, error) {
		getNode, err := s.kubeClient.Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
		if err != nil {
			klog.Errorf("Failed while getting a Node to retry updating node KubeletEndpoint Port, node: %s, error: %v", nodeName, err)
			return false, nil
		}

		getNode.Status.DaemonEndpoints.KubeletEndpoint.Port = int32(s.tunnelPort)
		_, err = s.kubeClient.Nodes().UpdateStatus(context.Background(), getNode, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("Failed to update node KubeletEndpoint Port, node: %s, tunnelPort: %d, err: %v", nodeName, s.tunnelPort, err)
			return false, nil
		}
		return true, nil
	}); err != nil {
		klog.Errorf("Update KubeletEndpoint Port of Node '%v' error: %v. ", nodeName, err)
		return fmt.Errorf("failed to Update KubeletEndpoint Port")
	}
	klog.V(4).Infof("Update node KubeletEndpoint Port successfully, node: %s, tunnelPort: %d", nodeName, s.tunnelPort)
	return nil
}
