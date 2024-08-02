// Original file obtained from https://github.com/kelseyhightower/scheduler/blob/master/kubernetes.go
// Unmodified
// -------------------------------------------------------------------------------------------
// Copyright 2016 Google Inc. All Rights Reserved.
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

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	apiHost           = "127.0.0.1:8001"
	bindingsEndpoint  = "/api/v1/namespaces/default/pods/%s/binding/"
	eventsEndpoint    = "/api/v1/namespaces/default/events"
	nodesEndpoint     = "/api/v1/nodes"
	podsEndpoint      = "/api/v1/pods"
	watchPodsEndpoint = "/api/v1/watch/pods"
)

func postEvent(event Event) error {
	var b []byte
	body := bytes.NewBuffer(b)
	err := json.NewEncoder(body).Encode(event)
	if err != nil {
		return err
	}
	
	request := &http.Request{
		Body:          ioutil.NopCloser(body),
		ContentLength: int64(body.Len()),
		Header:        make(http.Header),
		Method:        http.MethodPost,
		URL: &url.URL{
			Host:   apiHost,
			Path:   eventsEndpoint,
			Scheme: "http",
		},
	}
	request.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	if resp.StatusCode != 201 {
		return errors.New("Event: Unexpected HTTP status code" + resp.Status)
	}
	return nil
}

func getNodes() (*NodeList, error) {
	var nodeList NodeList

	request := &http.Request{
		Header: make(http.Header),
		Method: http.MethodGet,
		URL: &url.URL{
			Host:   apiHost,
			Path:   nodesEndpoint,
			Scheme: "http",
		},
	}
	request.Header.Set("Accept", "application/json, */*")

	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}

	err = json.NewDecoder(resp.Body).Decode(&nodeList)
	if err != nil {
		return nil, err
	}

	return &nodeList, nil
}

func watchUnscheduledPods() (<-chan Pod, <-chan error) {
	pods := make(chan Pod)
	errc := make(chan error, 1)

	// Sets up URL query parameters to filter pods that do not have a node assigned (spec.nodeName= is empty, meaning no node is assigned).
	v := url.Values{}
	v.Set("fieldSelector", "spec.nodeName=")

	// Creates an HTTP GET request to the endpoint specified by apiHost and watchPodsEndpoint, 
	// with the constructed query parameters and an Accept header indicating that JSON responses are expected.
	request := &http.Request{
		Header: make(http.Header),
		Method: http.MethodGet,
		URL: &url.URL{
			Host:     apiHost,
			Path:     watchPodsEndpoint,
			RawQuery: v.Encode(),
			Scheme:   "http",
		},
	}
	request.Header.Set("Accept", "application/json, */*")

	go func() {
		for {
			// Makes an HTTP request to the specified endpoint.
			resp, err := http.DefaultClient.Do(request)
			if err != nil {
				errc <- err
				time.Sleep(5 * time.Second)
				continue
			}

			if resp.StatusCode != 200 {
				errc <- errors.New("Invalid status code: " + resp.Status)
				time.Sleep(5 * time.Second)
				continue
			}

			decoder := json.NewDecoder(resp.Body)
			for {
				var event PodWatchEvent
				err = decoder.Decode(&event)
				if err != nil {
					errc <- err
					break
				}
				// If an event of type "ADDED" is received, it sends the Pod object to the pods channel.
				if event.Type == "ADDED" {
					pods <- event.Object
				}
			}
		}
	}()

	return pods, errc
}

func getUnscheduledPods() ([]*Pod, error) {
	var podList PodList
	unscheduledPods := make([]*Pod, 0)

	// Sets up URL query parameters to filter pods that do not have a node assigned (spec.nodeName= is empty, meaning no node is assigned).
	v := url.Values{}
	v.Set("fieldSelector", "spec.nodeName=")

	// makes an HTTP GET request to an API endpoint for pods, filtering based on spec.nodeName to get only unscheduled pods.
	request := &http.Request{
		Header: make(http.Header),
		Method: http.MethodGet,
		URL: &url.URL{
			Host:     apiHost,
			Path:     podsEndpoint,
			RawQuery: v.Encode(),
			Scheme:   "http",
		},
	}
	request.Header.Set("Accept", "application/json, */*")

	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return unscheduledPods, err
	}
	err = json.NewDecoder(resp.Body).Decode(&podList)
	if err != nil {
		return unscheduledPods, err
	}

	// Iterates through the list of pods (podList.Items).
	// Checks if the pod's annotations match the schedulerName. If they do, the pod is added to the unscheduledPods slice.
	for _, pod := range podList.Items {
		if pod.Metadata.Annotations["scheduler.alpha.kubernetes.io/name"] == schedulerName {
			unscheduledPods = append(unscheduledPods, &pod)
		}
	}

	return unscheduledPods, nil
}

func getPods() (*PodList, error) {
	var podList PodList

	v := url.Values{}
	v.Add("fieldSelector", "status.phase=Running")
	v.Add("fieldSelector", "status.phase=Pending")

	request := &http.Request{
		Header: make(http.Header),
		Method: http.MethodGet,
		URL: &url.URL{
			Host:     apiHost,
			Path:     podsEndpoint,
			RawQuery: v.Encode(),
			Scheme:   "http",
		},
	}
	request.Header.Set("Accept", "application/json, */*")

	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	err = json.NewDecoder(resp.Body).Decode(&podList)
	if err != nil {
		return nil, err
	}
	return &podList, nil
}

type ResourceUsage struct {
	CPU int
}

func fit(pod *Pod) ([]Node, error) {
	// Retrieve Nodes
	nodeList, err := getNodes()
	if err != nil {
		return nil, err
	}
	// Retrieve pods
	podList, err := getPods()
	if err != nil {
		return nil, err
	}

	// Initialize Resource Usage Tracking
	resourceUsage := make(map[string]*ResourceUsage)
	for _, node := range nodeList.Items {
		resourceUsage[node.Metadata.Name] = &ResourceUsage{}
	}

	// Calculate Current CPU Usage per Node
	for _, p := range podList.Items {
		if p.Spec.NodeName == "" {
			continue
		}
		for _, c := range p.Spec.Containers {
			if strings.HasSuffix(c.Resources.Requests["cpu"], "m") {
				milliCores := strings.TrimSuffix(c.Resources.Requests["cpu"], "m")
				cores, err := strconv.Atoi(milliCores)
				if err != nil {
					return nil, err
				}
				ru := resourceUsage[p.Spec.NodeName]
				ru.CPU += cores
			}
		}
	}

	var nodes []Node
	fitFailures := make([]string, 0)

	// Determine Required CPU for the New Pod
	var spaceRequired int
	for _, c := range pod.Spec.Containers {
		if strings.HasSuffix(c.Resources.Requests["cpu"], "m") {
			milliCores := strings.TrimSuffix(c.Resources.Requests["cpu"], "m")
			cores, err := strconv.Atoi(milliCores)
			if err != nil {
				return nil, err
			}
			spaceRequired += cores
		}
	}

	// Filter Nodes Based on Available CPU
	// For each node, calculate the allocatable CPU (handling both milliCores and non-milliCore values).
	for _, node := range nodeList.Items {
		var allocatableCores int
		var err error
		
		if strings.HasSuffix(node.Status.Allocatable["cpu"], "m") {
			milliCores := strings.TrimSuffix(node.Status.Allocatable["cpu"], "m")
			allocatableCores, err = strconv.Atoi(milliCores)
			if err != nil {
				return nil, err
			}
		} else {
			cpu := node.Status.Allocatable["cpu"]
			cpuFloat, err := strconv.ParseFloat(cpu, 32)
			if err != nil {
				return nil, err
			}
			allocatableCores = int(cpuFloat * 1000)
		}

		// Nodes that do not have sufficient resources are recorded in fitFailures and 
		// are not added to the list of suitable nodes.
		freeSpace := (allocatableCores - resourceUsage[node.Metadata.Name].CPU)
		if freeSpace < spaceRequired {
			m := fmt.Sprintf("fit failure on node (%s): Insufficient CPU", node.Metadata.Name)
			fitFailures = append(fitFailures, m)
			continue
		}
		nodes = append(nodes, node)
	}

	// If no nodes are suitable, it creates an Event object to record the scheduling failure.
	if len(nodes) == 0 {
		// Emit a Kubernetes event that the Pod was scheduled successfully.
		timestamp := time.Now().UTC().Format(time.RFC3339)
		event := Event{
			Count:          1,
			Message:        fmt.Sprintf("pod (%s) failed to fit in any node\n%s", pod.Metadata.Name, strings.Join(fitFailures, "\n")),
			Metadata:       Metadata{GenerateName: pod.Metadata.Name + "-"},
			Reason:         "FailedScheduling",
			LastTimestamp:  timestamp,
			FirstTimestamp: timestamp,
			Type:           "Warning",
			Source:         EventSource{Component: "hightower-scheduler"},
			InvolvedObject: ObjectReference{
				Kind:      "Pod",
				Name:      pod.Metadata.Name,
				Namespace: "default",
				Uid:       pod.Metadata.Uid,
			},
		}

		postEvent(event)
	}

	return nodes, nil
}

func bind(pod *Pod, node Node) error {
	binding := Binding{
		ApiVersion: "v1",
		Kind:       "Binding",
		Metadata:   Metadata{Name: pod.Metadata.Name},
		Target: Target{
			ApiVersion: "v1",
			Kind:       "Node",
			Name:       node.Metadata.Name,
		},
	}

	var b []byte
	body := bytes.NewBuffer(b)
	err := json.NewEncoder(body).Encode(binding)
	if err != nil {
		return err
	}

	request := &http.Request{
		Body:          ioutil.NopCloser(body),
		ContentLength: int64(body.Len()),
		Header:        make(http.Header),
		Method:        http.MethodPost,
		URL: &url.URL{
			Host:   apiHost,
			Path:   fmt.Sprintf(bindingsEndpoint, pod.Metadata.Name),
			Scheme: "http",
		},
	}
	request.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	if resp.StatusCode != 201 {
		return errors.New("Binding: Unexpected HTTP status code" + resp.Status)
	}

	// Emit a Kubernetes event that the Pod was scheduled successfully.
	message := fmt.Sprintf("Successfully assigned %s to %s", pod.Metadata.Name, node.Metadata.Name)
	timestamp := time.Now().UTC().Format(time.RFC3339)
	event := Event{
		Count:          1,
		Message:        message,
		Metadata:       Metadata{GenerateName: pod.Metadata.Name + "-"},
		Reason:         "Scheduled",
		LastTimestamp:  timestamp,
		FirstTimestamp: timestamp,
		Type:           "Normal",
		Source:         EventSource{Component: "hightower-scheduler"},
		InvolvedObject: ObjectReference{
			Kind:      "Pod",
			Name:      pod.Metadata.Name,
			Namespace: "default",
			Uid:       pod.Metadata.Uid,
		},
	}
	log.Println(message)
	return postEvent(event)
}
