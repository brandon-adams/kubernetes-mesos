package scheduler

import (
	"fmt"
	"time"

	"code.google.com/p/go-uuid/uuid"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	log "github.com/golang/glog"
	"github.com/mesos/mesos-go/mesos"
)

const (
	containerCpus = 0.25 // initial CPU allocated for executor
	containerMem  = 64   // initial MB of memory allocated for executor
)

// A struct that describes a pod task.
type PodTask struct {
	ID         string
	Pod        *api.Pod
	TaskInfo   *mesos.TaskInfo
	Offer      PerishableOffer
	launched   bool
	deleted    bool
	podKey     string
	createTime time.Time
	launchTime time.Time
	bindTime   time.Time
	mapper     hostPortMappingFunc
	ports      []hostPortMapping
}

func (t *PodTask) hasAcceptedOffer() bool {
	return t.TaskInfo != nil && t.TaskInfo.TaskId != nil
}

func (t *PodTask) GetOfferId() string {
	if t.Offer == nil {
		return ""
	}
	return t.Offer.Details().Id.GetValue()
}

// Fill the TaskInfo in the PodTask, should be called during k8s scheduling,
// before binding.
func (t *PodTask) FillFromDetails(details *mesos.Offer) error {
	if details == nil {
		//programming error
		panic("offer details are nil")
	}

	log.V(3).Infof("Recording offer(s) %v against pod %v", details.Id, t.Pod.Name)

	t.TaskInfo.TaskId = newTaskID(t.ID)
	t.TaskInfo.SlaveId = details.GetSlaveId()
	t.TaskInfo.Resources = []*mesos.Resource{
		mesos.ScalarResource("cpus", containerCpus),
		mesos.ScalarResource("mem", containerMem),
	}
	if mapping, err := t.mapper(t, details); err != nil {
		t.ClearTaskInfo()
		return err
	} else {
		ports := []uint64{}
		for _, entry := range mapping {
			ports = append(ports, entry.offerPort)
		}
		t.ports = mapping
		if portsResource := rangeResource("ports", ports); portsResource != nil {
			t.TaskInfo.Resources = append(t.TaskInfo.Resources, portsResource)
		}
	}
	return nil
}

// Clear offer-related details from the task, should be called if/when an offer
// has already been assigned to a task but for some reason is no longer valid.
func (t *PodTask) ClearTaskInfo() {
	log.V(3).Infof("Clearing offer(s) from pod %v", t.Pod.Name)
	t.Offer = nil
	t.TaskInfo.TaskId = nil
	t.TaskInfo.SlaveId = nil
	t.TaskInfo.Resources = nil
	t.TaskInfo.Data = nil
	t.ports = nil
}

func (t *PodTask) AcceptOffer(offer *mesos.Offer) bool {
	if offer == nil {
		return false
	}
	var (
		cpus float64 = 0
		mem  float64 = 0
	)
	for _, resource := range offer.Resources {
		if resource.GetName() == "cpus" {
			cpus = *resource.GetScalar().Value
		}

		if resource.GetName() == "mem" {
			mem = *resource.GetScalar().Value
		}
	}
	if _, err := t.mapper(t, offer); err != nil {
		log.V(3).Info(err)
		return false
	}
	if (cpus < containerCpus) || (mem < containerMem) {
		log.V(3).Infof("not enough resources: cpus: %f mem: %f", cpus, mem)
		return false
	}
	return true
}

// create a duplicate task, one that refers to the same pod specification and
// executor as the current task. all other state is reset to "factory settings"
// (as if returned from newPodTask)
func (t *PodTask) dup() (*PodTask, error) {
	ctx := api.WithNamespace(api.NewDefaultContext(), t.Pod.Namespace)
	return newPodTask(ctx, t.Pod, t.TaskInfo.Executor)
}

func newPodTask(ctx api.Context, pod *api.Pod, executor *mesos.ExecutorInfo) (*PodTask, error) {
	if pod == nil {
		return nil, fmt.Errorf("illegal argument: pod was nil")
	}
	if executor == nil {
		return nil, fmt.Errorf("illegal argument: executor was nil")
	}
	key, err := makePodKey(ctx, pod.Name)
	if err != nil {
		return nil, err
	}
	taskId := uuid.NewUUID().String()
	task := &PodTask{
		ID:       taskId,
		Pod:      pod,
		TaskInfo: newTaskInfo("PodTask"),
		podKey:   key,
		mapper:   defaultHostPortMapping,
	}
	task.TaskInfo.Executor = executor
	task.createTime = time.Now()
	return task, nil
}

type hostPortMapping struct {
	cindex    int // containerIndex
	pindex    int // portIndex
	offerPort uint64
}

// abstracts the way that host ports are mapped to pod container ports
type hostPortMappingFunc func(t *PodTask, offer *mesos.Offer) ([]hostPortMapping, error)

type PortAllocationError struct {
	PodId string
	Ports []uint64
}

func (err *PortAllocationError) Error() string {
	return fmt.Sprintf("Could not schedule pod %s: %d port(s) could not be allocated", err.PodId, len(err.Ports))
}

type DuplicateHostPortError struct {
	m1, m2 hostPortMapping
}

func (err *DuplicateHostPortError) Error() string {
	return fmt.Sprintf(
		"Host port %d is specified for container %d, pod %d and container %d, pod %d",
		err.m1.offerPort, err.m1.cindex, err.m1.pindex, err.m2.cindex, err.m2.pindex)
}

// default k8s host port mapping implementation: hostPort == 0 means containerPort remains pod-private
func defaultHostPortMapping(t *PodTask, offer *mesos.Offer) ([]hostPortMapping, error) {
	requiredPorts := make(map[uint64]hostPortMapping)
	mapping := []hostPortMapping{}
	if t.Pod == nil {
		// programming error
		panic("task.Pod is nil")
	}
	for i, container := range t.Pod.Spec.Containers {
		// strip all port==0 from this array; k8s already knows what to do with zero-
		// ports (it does not create 'port bindings' on the minion-host); we need to
		// remove the wildcards from this array since they don't consume host resources
		for pi, port := range container.Ports {
			if port.HostPort == 0 {
				continue // ignore
			}
			m := hostPortMapping{
				cindex:    i,
				pindex:    pi,
				offerPort: uint64(port.HostPort),
			}
			if entry, inuse := requiredPorts[uint64(port.HostPort)]; inuse {
				return nil, &DuplicateHostPortError{entry, m}
			}
			requiredPorts[uint64(port.HostPort)] = m
		}
	}
	for _, resource := range offer.Resources {
		if resource.GetName() == "ports" {
			for _, r := range (*resource).GetRanges().Range {
				bp := r.GetBegin()
				ep := r.GetEnd()
				for port, _ := range requiredPorts {
					log.V(3).Infof("evaluating port range {%d:%d} %d", bp, ep, port)
					if (bp <= port) && (port <= ep) {
						mapping = append(mapping, requiredPorts[port])
						delete(requiredPorts, port)
					}
				}
			}
		}
	}
	unsatisfiedPorts := len(requiredPorts)
	if unsatisfiedPorts > 0 {
		err := &PortAllocationError{
			PodId: t.Pod.Name,
		}
		for p, _ := range requiredPorts {
			err.Ports = append(err.Ports, p)
		}
		return nil, err
	}
	return mapping, nil
}
