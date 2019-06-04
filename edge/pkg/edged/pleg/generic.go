package pleg

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"sort"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apimachinery/pkg/util/wait"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/pleg"
	"k8s.io/kubernetes/pkg/kubelet/prober"
	"k8s.io/kubernetes/pkg/kubelet/status"

	"github.com/kubeedge/beehive/pkg/common/log"
	"github.com/kubeedge/kubeedge/edge/pkg/edged/containers"
	"github.com/kubeedge/kubeedge/edge/pkg/edged/podmanager"
)

//GenericLifecycle is object for pleg lifecycle
type GenericLifecycle struct {
	pleg.GenericPLEG
	runtime      containers.ContainerManager
	relistPeriod time.Duration
	status       status.Manager
	podManager   podmanager.Manager
	probeManager prober.Manager
	podCache     kubecontainer.Cache
	remoteRuntime kubecontainer.Runtime
	clock clock.Clock
}

//NewGenericLifecycle creates new generic life cycle object
func NewGenericLifecycle(manager containers.ContainerManager, probeManager prober.Manager, channelCapacity int,
	relistPeriod time.Duration, podManager podmanager.Manager, statusManager status.Manager) pleg.PodLifecycleEventGenerator {
	kubeContainerManager := containers.NewKubeContainerRuntime(manager)
	genericPLEG := pleg.NewGenericPLEG(kubeContainerManager, channelCapacity, relistPeriod, nil, clock.RealClock{})
	return &GenericLifecycle{
		GenericPLEG:  *genericPLEG.(*pleg.GenericPLEG),
		relistPeriod: relistPeriod,
		runtime:      manager,
		status:       statusManager,
		podCache: nil,
		podManager:   podManager,
		probeManager: probeManager,
		remoteRuntime: nil,
		clock: clock.RealClock{},
	}
}//NewGenericLifecycle creates new generic life cycle object
func NewGenericLifecycleRemote(runtime kubecontainer.Runtime, probeManager prober.Manager, channelCapacity int,
	relistPeriod time.Duration, podManager podmanager.Manager, statusManager status.Manager, podCache kubecontainer.Cache, clock clock.Clock) pleg.PodLifecycleEventGenerator {
	//kubeContainerManager := containers.NewKubeContainerRuntime(manager)
	genericPLEG := pleg.NewGenericPLEG(runtime, channelCapacity, relistPeriod, podCache, clock)
	return &GenericLifecycle{
		GenericPLEG:  	*genericPLEG.(*pleg.GenericPLEG),
		relistPeriod: 	relistPeriod,
		remoteRuntime:  runtime,
		status:       	statusManager,
		podCache: 		podCache,
		podManager:   	podManager,
		probeManager: 	probeManager,
		runtime: 		nil,
		clock: 			clock,
	}
}



// Start spawns a goroutine to relist periodically.
func (gl *GenericLifecycle) Start() {
	gl.GenericPLEG.Start()
	go wait.Until(func() {
		log.LOGGER.Infof("GenericLifecycle: Relisting")
		podListPm := gl.podManager.GetPods()
		for _, pod := range podListPm {
			if err := gl.updatePodStatus(pod); err != nil {
				log.LOGGER.Errorf("update pod %s status error", pod.Name)
			}
		}
	}, gl.relistPeriod, wait.NeverStop)
}

// convertStatusToAPIStatus creates an api PodStatus for the given pod from
// the given internal pod status.  It is purely transformative and does not
// alter the kubelet state at all.
func (gl *GenericLifecycle) convertStatusToAPIStatus(pod *v1.Pod, podStatus *kubecontainer.PodStatus) *v1.PodStatus {
	var apiPodStatus v1.PodStatus
	apiPodStatus.PodIP = podStatus.IP
	// set status for Pods created on versions of kube older than 1.6
	apiPodStatus.QOSClass = v1qos.GetPodQOS(pod)

	oldPodStatus, found := gl.status.GetPodStatus(pod.UID)
	if !found {
		oldPodStatus = pod.Status
	}

	apiPodStatus.ContainerStatuses = gl.convertToAPIContainerStatuses(
		pod, podStatus,
		oldPodStatus.ContainerStatuses,
		pod.Spec.Containers,
		len(pod.Spec.InitContainers) > 0,
		false,
	)
	apiPodStatus.InitContainerStatuses = gl.convertToAPIContainerStatuses(
		pod, podStatus,
		oldPodStatus.InitContainerStatuses,
		pod.Spec.InitContainers,
		len(pod.Spec.InitContainers) > 0,
		true,
	)

	// Preserves conditions not controlled by kubelet
	for _, c := range pod.Status.Conditions {
		//if !kubetypes.PodConditionByKubelet(c.Type) {
			apiPodStatus.Conditions = append(apiPodStatus.Conditions, c)
		//}
	}
	return &apiPodStatus
}

// convertToAPIContainerStatuses converts the given internal container
// statuses into API container statuses.
func (gl *GenericLifecycle) convertToAPIContainerStatuses(pod *v1.Pod, podStatus *kubecontainer.PodStatus, previousStatus []v1.ContainerStatus, containers []v1.Container, hasInitContainers, isInitContainer bool) []v1.ContainerStatus {
	convertContainerStatus := func(cs *kubecontainer.ContainerStatus) *v1.ContainerStatus {
		cid := cs.ID.String()
		cstatus := &v1.ContainerStatus{
			Name:         cs.Name,
			RestartCount: int32(cs.RestartCount),
			Image:        cs.Image,
			ImageID:      cs.ImageID,
			ContainerID:  cid,
		}
		switch cs.State {
		case kubecontainer.ContainerStateRunning:
			cstatus.State.Running = &v1.ContainerStateRunning{StartedAt: metav1.NewTime(cs.StartedAt)}
		case kubecontainer.ContainerStateCreated:
			// Treat containers in the "created" state as if they are exited.
			// The pod workers are supposed start all containers it creates in
			// one sync (syncPod) iteration. There should not be any normal
			// "created" containers when the pod worker generates the status at
			// the beginning of a sync iteration.
			fallthrough
		case kubecontainer.ContainerStateExited:
			cstatus.State.Terminated = &v1.ContainerStateTerminated{
				ExitCode:    int32(cs.ExitCode),
				Reason:      cs.Reason,
				Message:     cs.Message,
				StartedAt:   metav1.NewTime(cs.StartedAt),
				FinishedAt:  metav1.NewTime(cs.FinishedAt),
				ContainerID: cid,
			}
		default:
			cstatus.State.Waiting = &v1.ContainerStateWaiting{}
		}
		return cstatus
	}

	// Fetch old containers statuses from old pod status.
	oldStatuses := make(map[string]v1.ContainerStatus, len(containers))
	for _, cstatus := range previousStatus {
		oldStatuses[cstatus.Name] = cstatus
	}

	// Set all container statuses to default waiting state
	statuses := make(map[string]*v1.ContainerStatus, len(containers))
	defaultWaitingState := v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "ContainerCreating"}}
	if hasInitContainers {
		defaultWaitingState = v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "PodInitializing"}}
	}

	for _, container := range containers {
		cstatus := &v1.ContainerStatus{
			Name:  container.Name,
			Image: container.Image,
			State: defaultWaitingState,
		}
		oldStatus, found := oldStatuses[container.Name]
		if found {
			if oldStatus.State.Terminated != nil {
				// Do not update status on terminated init containers as
				// they be removed at any time.
				cstatus = &oldStatus
			} else {
				// Apply some values from the old statuses as the default values.
				cstatus.RestartCount = oldStatus.RestartCount
				cstatus.LastTerminationState = oldStatus.LastTerminationState
			}
		}
		statuses[container.Name] = cstatus
	}

	// Make the latest container status comes first.
	sort.Sort(sort.Reverse(kubecontainer.SortContainerStatusesByCreationTime(podStatus.ContainerStatuses)))
	// Set container statuses according to the statuses seen in pod status
	containerSeen := map[string]int{}
	for _, cStatus := range podStatus.ContainerStatuses {
		cName := cStatus.Name
		if _, ok := statuses[cName]; !ok {
			// This would also ignore the infra container.
			continue
		}
		if containerSeen[cName] >= 2 {
			continue
		}
		cstatus := convertContainerStatus(cStatus)
		if containerSeen[cName] == 0 {
			statuses[cName] = cstatus
		} else {
			statuses[cName].LastTerminationState = cstatus.State
		}
		containerSeen[cName] = containerSeen[cName] + 1
	}

	// Handle the containers failed to be started, which should be in Waiting state.
	for _, container := range containers {
		if isInitContainer {
			// If the init container is terminated with exit code 0, it won't be restarted.
			// TODO(random-liu): Handle this in a cleaner way.
			s := podStatus.FindContainerStatusByName(container.Name)
			if s != nil && s.State == kubecontainer.ContainerStateExited && s.ExitCode == 0 {
				continue
			}
		}
		// If a container should be restarted in next syncpod, it is *Waiting*.
		if !kubecontainer.ShouldContainerBeRestarted(&container, pod, podStatus) {
			continue
		}
		cstatus := statuses[container.Name]
		/*reason, ok := kl.reasonCache.Get(pod.UID, container.Name)
		if !ok {
			// In fact, we could also apply Waiting state here, but it is less informative,
			// and the container will be restarted soon, so we prefer the original state here.
			// Note that with the current implementation of ShouldContainerBeRestarted the original state here
			// could be:
			//   * Waiting: There is no associated historical container and start failure reason record.
			//   * Terminated: The container is terminated.
			continue
		}*/
		if cstatus.State.Terminated != nil {
			cstatus.LastTerminationState = cstatus.State
		}
		/*cstatus.State = v1.ContainerState{
			Waiting: &v1.ContainerStateWaiting{
				Reason:  reason.Err.Error(),
				Message: reason.Message,
			},
		}*/
		statuses[container.Name] = cstatus
	}

	var containerStatuses []v1.ContainerStatus
	for _, cstatus := range statuses {
		containerStatuses = append(containerStatuses, *cstatus)
	}

	// Sort the container statuses since clients of this interface expect the list
	// of containers in a pod has a deterministic order.
	if isInitContainer {
		kubetypes.SortInitContainerStatuses(pod, containerStatuses)
	} else {
		sort.Sort(kubetypes.SortedContainerStatuses(containerStatuses))
	}
	return containerStatuses
}


func (gl *GenericLifecycle) updatePodStatus(pod *v1.Pod) error {
	var podStatus *v1.PodStatus
	var newStatus v1.PodStatus
	var podStatusRemote *kubecontainer.PodStatus
	//var newStatusRemote kubecontainer.PodStatus
	var err error
	if gl.runtime != nil {
		podStatus, err = gl.runtime.GetPodStatusOwn(pod)

	}
	if gl.remoteRuntime != nil {
		podStatusRemote, err = gl.remoteRuntime.GetPodStatus(pod.UID, pod.Name, pod.Namespace)
		/*if err != nil {
			glog.Errorf("Unable to get runtime pod status for pod %s", pod.Name)
			return err
		}*/
		//glog.Errorf("Pod status for pod %s is [%s]", pod.Name, podStatusRemote)
		/*newStatusRemote.Name = podStatusRemote.Name
		newStatusRemote.Namespace = podStatusRemote.Namespace
		newStatusRemote.ID = podStatusRemote.ID
		newStatusRemote.IP = podStatusRemote.IP
		for _, cstatus := range podStatusRemote.ContainerStatuses {
			newStatusRemote.ContainerStatuses = append(newStatusRemote.ContainerStatuses,cstatus)
		}
		for _, sstatus := range podStatusRemote.SandboxStatuses {
			newStatusRemote.SandboxStatuses = append(newStatusRemote.SandboxStatuses,sstatus)
		}
		timestamp := gl.clock.Now()
		gl.podCache.Set(pod.UID, &newStatusRemote, err, timestamp)*/
		podStatus = gl.convertStatusToAPIStatus(pod, podStatusRemote)
	}
	if err != nil {
		return err
	}
	newStatus = *podStatus.DeepCopy()

	gl.probeManager.UpdatePodStatus(pod.UID, &newStatus)
	if gl.runtime != nil {
		newStatus.Conditions = append(newStatus.Conditions, gl.runtime.GeneratePodReadyCondition(newStatus.ContainerStatuses))
	}
	if gl.remoteRuntime != nil {
		newStatus.Conditions = append(newStatus.Conditions, status.GeneratePodReadyCondition(&pod.Spec, newStatus.ContainerStatuses, pod.Status.Phase))
	}
	pod.Status = newStatus
	gl.status.SetPodStatus(pod, newStatus)
	return err
}

func (gl *GenericLifecycle) GetPodStatus(pod *v1.Pod) (v1.PodStatus, bool) {
	return gl.status.GetPodStatus(pod.UID)
}

func  GetRemotePodStatus (gl *GenericLifecycle, podUID types.UID) (*kubecontainer.PodStatus, error) {
	return gl.podCache.Get(podUID)
}