/*
Copyright 2019 The Kubernetes Authors.

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

package realtime

import (
	"context"
	"errors"
	"math"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// RealTime is a plugin that checks if the RT utilization of the current node is lower than the threshold for the current pod.
type RealTime struct {
	handle framework.Handle
}

// RealTimeWCET and RealTimeData are two structs to extract the data from the RealTime CRD
type RealTimeWCET struct {
	Node   string `json:"node,omitempty"`
	RTWcet int    `json:"rtWcet,omitempty"`
}

type RealTimeData struct {
	// +optional
	Criticality string         `json:"criticality,omitempty"`
	RTDeadline  int            `json:"rtDeadline,omitempty"`
	RTPeriod    int            `json:"rtPeriod,omitempty"`
	RTWcets     []RealTimeWCET `json:"rtWcets,omitempty"`
}

var _ framework.FilterPlugin = &RealTime{}
var _ framework.ScorePlugin = &RealTime{}
var _ framework.QueueSortPlugin = &RealTime{}

const (
	// Name is the name of the plugin used in the plugin registry and configurations.
	Name      = "RealTime"
	ErrReason = "node(s) didn't have enough RT resources"
)

// EventsToRegister returns the possible events that may make a Pod
// failed by this plugin schedulable.
func (pl *RealTime) EventsToRegister() []framework.ClusterEvent {
	return []framework.ClusterEvent{
		{Resource: framework.Node, ActionType: framework.Add},
	}
}

// Name returns name of the plugin. It is used in logs, etc.
func (pl *RealTime) Name() string {
	return Name
}

// Sort the Pod queue by criticality, if two pods have the same criticality, take the one with the earlier timestamp
// If the pod has no criticality, the RT one is scheduled first
// If both pods have no criticality, then we use priority
func (pl *RealTime) Less(pInfo1, pInfo2 *framework.QueuedPodInfo) bool {
	realTimeDataP1, _ := GetRealTimeData(context.TODO(), pInfo1.Pod)
	realTimeDataP2, _ := GetRealTimeData(context.TODO(), pInfo2.Pod)

	criticality_p1 := realTimeDataP1.Criticality
	criticality_p2 := realTimeDataP2.Criticality

	if criticality_p1 == "N" && criticality_p2 == "N" {
		p1 := corev1helpers.PodPriority(pInfo1.Pod)
		p2 := corev1helpers.PodPriority(pInfo2.Pod)
		return (p1 > p2) || (p1 == p2 && pInfo1.Timestamp.Before(pInfo2.Timestamp))
	} else if criticality_p1 != "N" && criticality_p2 == "N" {
		return true
	} else if criticality_p1 == "N" && criticality_p2 != "N" {
		return false
	}
	return (criticality_p1 > criticality_p2) || (criticality_p1 == criticality_p2 && pInfo1.Timestamp.Before(pInfo2.Timestamp))
}

func GetResourcesDynamically(dynamic dynamic.Interface, ctx context.Context, group string, version string, resource string, namespace string) ([]unstructured.Unstructured, error) {

	resourceId := schema.GroupVersionResource{
		Group:    group,
		Version:  version,
		Resource: resource,
	}
	time.Sleep(time.Duration(1) * time.Second) // Wait for CRD to be created
	list, err := dynamic.Resource(resourceId).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	return list.Items, nil
}

func GetRealTimeData(ctx context.Context, pod *v1.Pod) (RealTimeData, error) {
	matchAppName := pod.Labels["scheduling.francescol96.univr"]
	config, err := rest.InClusterConfig()
	if err != nil {
		return RealTimeData{Criticality: "N"}, errors.New("could not obtain incluster config")
	}
	dynamic := dynamic.NewForConfigOrDie(config)
	items, err := GetResourcesDynamically(dynamic, ctx, "scheduling.francescol96.univr", "v1", "realtimes", "default")

	if err != nil {
		return RealTimeData{Criticality: "N"}, err
	} else {
		for _, item := range items {
			typedData := RealTimeData{}
			appName, appNameFound, appNameErr := unstructured.NestedString(item.Object, "metadata", "name")

			if !appNameFound || appNameErr != nil {
				return RealTimeData{Criticality: "N"}, appNameErr
			}
			if matchAppName == appName {
				criticality, criticalityFound, criticalityErr := unstructured.NestedString(item.Object, "spec", "criticality")
				rtDeadline, rtDeadlineFound, rtDeadlineErr := unstructured.NestedInt64(item.Object, "spec", "rtDeadline")
				rtPeriod, rtPeriodFound, rtPeriodErr := unstructured.NestedInt64(item.Object, "spec", "rtPeriod")

				if criticalityFound && criticalityErr == nil {
					typedData.Criticality = criticality
				} else {
					return RealTimeData{Criticality: "N"}, criticalityErr
				}

				if rtDeadlineFound && rtDeadlineErr == nil {
					typedData.RTDeadline = int(rtDeadline)
				} else {
					return RealTimeData{Criticality: "N"}, rtDeadlineErr
				}

				if rtPeriodFound && rtPeriodErr == nil {
					typedData.RTPeriod = int(rtPeriod)
				} else {
					return RealTimeData{Criticality: "N"}, rtPeriodErr
				}

				rtWcets, rtWcetsFound, rtWcetsErr := unstructured.NestedSlice(item.Object, "spec", "rtWcets")
				if rtWcetsFound && rtWcetsErr == nil {
					rtWcetsArray := []RealTimeWCET{}
					for _, rtWcet := range rtWcets {
						mapRTWcet, ok := rtWcet.(map[string]interface{})
						if !ok {
							return RealTimeData{Criticality: "N"}, errors.New("unable to obtain map from rtWcet object")
						}
						rtWcetsArray = append(rtWcetsArray, RealTimeWCET{Node: mapRTWcet["node"].(string), RTWcet: int(mapRTWcet["rtWcet"].(int64))})
					}
					typedData.RTWcets = append(typedData.RTWcets, rtWcetsArray...)
				} else {
					return RealTimeData{Criticality: "N"}, rtWcetsErr
				}

				/* // Log information
				klog.Infof("Data")
				klog.Infof("Object name: %s", appName)
				klog.Infof("Criticality: %s", typedData.Criticality)
				klog.Infof("RTDeadline: %d", typedData.RTDeadline)
				klog.Infof("RTPeriod: %d", typedData.RTPeriod)
				klog.Infof("RTWcets: ")
				for _, rtWcet := range typedData.RTWcets {
					klog.Infof("\tNode: %s", rtWcet.Node)
					klog.Infof("\tRTWcet: %d", rtWcet.RTWcet)
				}*/
				return typedData, nil
			}
		}
	}
	return RealTimeData{Criticality: "N"}, errors.New("GetRealTimeData: something went wrong: final return")
}

// Filter invoked at the filter extension point.
func (pl *RealTime) Filter(ctx context.Context, _ *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	if nodeInfo.Node() == nil {
		return framework.NewStatus(framework.Error, "Node not found")
	}

	realTimeData, err := GetRealTimeData(ctx, pod)
	if err != nil {
		return framework.NewStatus(framework.Error, "Filter: error while retrieving CRD list for RealTime or typing data: %s", err.Error())
	}

	// Pod is not Real-Time
	if realTimeData.Criticality == "" {
		return nil
	}

	// 0 All good
	// 1 Criticality mismatch
	// 2 Label missing
	// 3 Utilization calculation error
	// 4 Utilization higher than threshold
	errFits := Fits(pod, nodeInfo, realTimeData)
	if errFits == 1 {
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "Criticality mismatch")
	} else if errFits == 2 {
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "Real-Time label missing")
	} else if errFits == 3 {
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "Utilization calculation error")
	} else if errFits == 4 {
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "Utilization higher than threshold")
	}
	return nil
}

func (pl *RealTime) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	realTimeData, err := GetRealTimeData(ctx, pod)
	if err != nil {
		return 0, framework.NewStatus(framework.Error, "Score: error while retrieving CRD list for RealTime or typing data: %s", err.Error())
	}
	nodeInfo, _ := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	newNodeUT := GetNodeNewUT(pod, nodeInfo, realTimeData)
	thresholdUT := GetNodeThreshold(nodeInfo)
	nRank := (thresholdUT - newNodeUT) / thresholdUT

	if realTimeData.Criticality == "A" {
		return 100 - int64(nRank*100), nil
	} else if realTimeData.Criticality == "C" {
		return int64(nRank * 100), nil
	} else {
		nRank = 1 - nRank
		if nRank < 0.5 {
			return int64((0.9 + 0.2*nRank) * 100), nil
		} else {
			return int64(((math.Pow(nRank, 2) * (5.0 / 3.0)) - nRank*(9.0/2.0) + (17.0 / 6.0)) * 100), nil
		}
	}
}

func (pl *RealTime) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	// Scores are already normalized due to how they are constructed
	return nil
}

// ScoreExtensions of the Score plugin.
func (pl *RealTime) ScoreExtensions() framework.ScoreExtensions {
	return pl
}

// Fits actually checks if the pod fits the node.
// 0 All good
// 1 Criticality mismatch
// 2 Label missing
// 3 Utilization calculation error
// 4 Utilization higher than threshold
func Fits(pod *v1.Pod, nodeInfo *framework.NodeInfo, realTimeData RealTimeData) int {
	// Check whether the node has the RealTimeCriticality label
	if val, ok := nodeInfo.Node().ObjectMeta.Labels["RealTimeCriticality"]; ok {
		// If the pod needs criticality C and the node does not have criticality C, then it is not schedulable
		// If the node isolates criticality C and the pod is not criticality C, then it is not schedulable
		// It's an XOR between the two conditions
		if (realTimeData.Criticality == "C") != (val == "C") {
			return 1
		}
	} else { // If it does not have the RTc label, then it is not RT compliant
		return 2
	}

	// Check the utilization
	newNodeUT := GetNodeNewUT(pod, nodeInfo, realTimeData)
	thresholdUT := GetNodeThreshold(nodeInfo)
	if newNodeUT < 0 {
		return 3
	}
	if newNodeUT > thresholdUT {
		return 4
	}
	return 0
}

func GetNodeThreshold(nodeInfo *framework.NodeInfo) float64 {
	resQuant := nodeInfo.Node().Status.Capacity[v1.ResourceCPU]
	resQuantInt, _ := resQuant.AsInt64()
	return 0.95 * float64(resQuantInt)
}

func GetNodeNewUT(pod *v1.Pod, nodeInfo *framework.NodeInfo, realTimeData RealTimeData) float64 {
	// We use the deadline to calculate the utilization instead of the period as a worst-case scenario
	nodeUTcurrent := 0.0

	for _, podInfo_in_node := range nodeInfo.Pods {
		// If the Pod does not have RealTimeData, then it is not RT, thus we continue with the next pod
		if _, ok := podInfo_in_node.Pod.Labels["scheduling.francescol96.univr"]; !ok {
			continue
		}
		realTimeData, err := GetRealTimeData(context.TODO(), podInfo_in_node.Pod)
		if err != nil {
			klog.Error("GetNodeNewUT: Error while obtaining realTimeData for pod: %s; %s", podInfo_in_node.Pod.Name, err)
			return float64(-1)
		}
		if realTimeData.RTDeadline > realTimeData.RTPeriod {
			klog.Error("GetNodeNewUT: RTDeadline > RTPeriod for pod: %s", podInfo_in_node.Pod.Name)
			return float64(-1)
		}
		for _, wcet := range realTimeData.RTWcets {
			if wcet.Node == nodeInfo.Node().Name {
				if realTimeData.RTDeadline >= wcet.RTWcet {
					nodeUTcurrent += float64(wcet.RTWcet) / float64(realTimeData.RTDeadline)
				} else {
					klog.Error("RTWcet > RTDeadline for pod: %s", podInfo_in_node.Pod.Name)
					return float64(-1)
				}
			}
		}
	}

	// We do not need to check if the RT values are !=0 because this function is called only for RT deployments
	// We return the new utilization using the WCET for the current node
	// If we do not find it, we use the worst WCET out of all listed WCETs
	worstWCET := float64(-1)
	for _, wcet := range realTimeData.RTWcets {
		if wcet.Node == nodeInfo.Node().Name {
			if float64(wcet.RTWcet) > float64(realTimeData.RTDeadline) {
				return float64(-1)
			}
			return nodeUTcurrent + float64(wcet.RTWcet)/float64(realTimeData.RTDeadline)
		}
		if float64(wcet.RTWcet) > float64(worstWCET) {
			worstWCET = float64(wcet.RTWcet)
		}
	}
	// If we don't find the WCET for the current node, we return the utilization for the worst WCET
	return worstWCET + nodeUTcurrent
}

// New initializes a new plugin and returns it.
func New(_ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &RealTime{handle: h}, nil
}
