package clcm

import (
	"github.com/golang/glog"
	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager"
	"time"
)

const (
	//PolicyNone define no policy
	PolicyNone      = "none"
	reconcilePeriod = 1 * time.Second
)

type containerLifecycleManagerImpl struct {
	cpuManager cpumanager.Manager
}

var _ ContainerLifecycleManager = &containerLifecycleManagerImpl{}

// NewContainerLifecycleManager create new object for container lifecycle manager
func NewContainerLifecycleManager(kubeletRootDir string) (ContainerLifecycleManager, error) {
	var err error
	clcm := &containerLifecycleManagerImpl{}
	result := make(v1.ResourceList)
	clcm.cpuManager, err = cpumanager.NewManager(
		PolicyNone,
		reconcilePeriod,
		nil,
		result,
		kubeletRootDir,
	)
	if err != nil {
		glog.Errorf("failed to initialize cpu manager: %v", err)
		return nil, err
	}
	return clcm, nil
}

func (clcm *containerLifecycleManagerImpl) InternalContainerLifecycle() InternalContainerLifecycle {
	return &internalContainerLifecycleImpl{clcm.cpuManager}
}
