package clcm

// ContainerLifecycleManager interface
type ContainerLifecycleManager interface {
	InternalContainerLifecycle() InternalContainerLifecycle
}
