/*
Copyright (c) 2018 The ZJU-SEL Authors.

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

package helm

import (
	"io/ioutil"
	"os"
	"path"
	"strings"
	"time"

	"github.com/ZJU-SEL/capstan/pkg/capstan/types"
	"github.com/ZJU-SEL/capstan/pkg/util"
	"github.com/ZJU-SEL/capstan/pkg/workload"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	apismetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// TestingTool represents a testing tool which used to test a workload managed by helm.
type TestingTool struct {
	Workload       *Workload
	Name           string
	Script         string
	Steps          time.Duration
	CurrentTesting workload.TestingCase
	TestingCaseSet []workload.TestingCase
}

// Ensure the testing tool implements workload.Tool interface.
var _ workload.Tool = &TestingTool{}

// Run runs the defined testing case set of the testing tool (to adhere to workload.Tool interface).
func (t *TestingTool) Run(kubeClient kubernetes.Interface, testingCase workload.TestingCase) error {
	t.CurrentTesting = testingCase

	// 1. start a workload for the testing case.
	ret, err := util.RunCommand("helm", "install", "--name", t.Workload.Helm.Name, "--set", t.Workload.Helm.Set, "--namespace", workload.Namespace, t.Workload.Helm.Chart)
	if err != nil {
		return errors.Errorf("helm install failed, ret:%s, error:%v", strings.Join(ret, "\n"), err)
	}

	// 2. TODO(mozhuli): add more rules to check workload is available or not,
	// now we only check the deployment of the workload is available or not.
	glog.V(4).Infof("Check the deployment of %s workload is available or not", t.Workload.Name)
	err = workload.CheckDeployment(kubeClient, t.Workload.Helm.Name+"-"+t.Workload.Name)
	if err != nil {
		return errors.Wrapf(err, "unable to check the deployment of %s workload is available or not for testing case %s", t.Workload.Name, testingCase.Name)
	}

	// 3. create ConfigMaps which is used by the testing pod.
	// 3.1 create capstan-script configmap.
	err = workload.CreateConfigMapFromFile(kubeClient, t.Script)
	if err != nil {
		return errors.Wrapf(err, "failed create configmap from file %s", t.Script)
	}

	// 3.2 create capstan-envs configmap.
	data := parseEnvs(testingCase.Envs)

	labelData := map[string]string{
		"job":          t.Workload.Name,
		"uid":          types.UUID,
		"provider":     types.Provider,
		"startTime":    time.Now().Format("2006-01-02 15:04:05"),
		"workloadName": t.Workload.Name,
		"testCase":     t.GetName(),
	}
	data["PrometheusLabel"] = megeLabel(labelData)

	err = workload.CreateConfigMap(kubeClient, "capstan-envs", data)
	if err != nil {
		return errors.Wrapf(err, "failed create configmap from map %s", data)
	}

	// 4. start a testing pod for testing the workload.
	testingPodName := workload.BuildTestingPodName(t.GetName(), testingCase.Name)
	testingPod, args := t.findTemplate(testingCase.Name)

	tempTestingArgs := struct{ Name, Namespace, TestingName, Image, Label, Args string }{
		Name:        testingPodName,
		Namespace:   workload.Namespace,
		TestingName: testingCase.Name,
		Image:       "wadelee/capstan-base",
		Label:       t.Workload.Helm.Name + "-" + t.Workload.Name,
		Args:        workload.FomatArgs(args),
	}

	testingPodBytes, err := workload.ParseTemplate(testingPod, tempTestingArgs)
	if err != nil {
		return errors.Wrapf(err, "unable to parse %v using %v", testingPod, tempTestingArgs)
	}

	glog.V(4).Infof("Creating testing pod %q of testing case %s", testingPodName, testingCase.Name)
	if err := workload.CreatePod(kubeClient, testingPodBytes); err != nil {
		return errors.Wrapf(err, "unable to create the testing pod for testing case %s", testingCase.Name)
	}

	return nil
}

// HasTestingDone checks the testing case has finished or not(use the finish mark "Capstan Testing Done"). (to adhere to workload.Tool interface).
func (t *TestingTool) HasTestingDone(kubeClient kubernetes.Interface) error {
	name := workload.BuildTestingPodName(t.GetName(), t.CurrentTesting.Name)
	for {
		// Sleep between each poll
		// TODO(mozhuli): Use a watcher instead of polling.
		time.Sleep(30 * time.Second)

		// Make sure there's a pod.
		pod, err := kubeClient.CoreV1().Pods(workload.Namespace).Get(name, apismetav1.GetOptions{})
		if err != nil {
			return errors.WithStack(err)
		}

		// Make sure the pod isn't failing.
		if isFailing, err := workload.IsPodFailing(pod); isFailing {
			return err
		}

		// Check the test has done.
		body, err := kubeClient.CoreV1().Pods(workload.Namespace).GetLogs(
			name,
			&v1.PodLogOptions{},
		).Do().Raw()

		if err != nil {
			return errors.WithStack(err)
		}

		glog.V(5).Infof("Checking testing has done:\n%s", string(body))
		if workload.HasTestingDone(body) {
			glog.V(4).Infof("Testing case %s has done", t.CurrentTesting.Name)

			// export to capstan result directory.
			outdir := path.Join(types.ResultsDir, types.UUID, "workloads", t.Workload.Name, t.GetName(), t.CurrentTesting.Name)
			if err = os.MkdirAll(outdir, 0755); err != nil {
				return errors.WithStack(err)
			}

			outfile := path.Join(outdir, t.GetName()) + ".log"
			if err = ioutil.WriteFile(outfile, body, 0644); err != nil {
				return errors.WithStack(err)
			}

			return nil
		}
	}
}

// Cleanup cleans up all resources created by a testing case for mysql testing tool (to adhere to workload.Tool interface).
func (t *TestingTool) Cleanup(kubeClient kubernetes.Interface) error {
	// Delete testing pod.
	if err := workload.DeletePod(kubeClient, workload.BuildTestingPodName(t.GetName(), t.CurrentTesting.Name)); err != nil {
		return err
	}
	// Delete the release of the workload
	ret, err := util.RunCommand("helm", "delete", "--purge", t.Workload.Helm.Name)
	if err != nil {
		return errors.Errorf("helm install failed, ret:%s, error:%v", strings.Join(ret, "\n"), err)
	}
	return nil
}

// GetName returns the name of the testing tool (to adhere to workload.Tool interface).
func (t *TestingTool) GetName() string {
	return t.Name
}

// GetSteps returns the steps between each testing case (to adhere to workload.Tool interface).
func (t *TestingTool) GetSteps() time.Duration {
	return t.Steps
}

// GetTestingCaseSet returns the testing case set which the testing tool will run (to adhere to workload.Tool interface).
func (t *TestingTool) GetTestingCaseSet() []workload.TestingCase {
	return t.TestingCaseSet
}

// findTemplate returns the true testing tool template and arguments for different testing cases.
func (t *TestingTool) findTemplate(name string) (string, string) {
	if t.CurrentTesting.Affinity {
		return PodAffinity, t.CurrentTesting.Args
	}
	return PodAntiAffinity, t.CurrentTesting.Args
}

// parseEnvs parse string to map[string]string
func parseEnvs(raw string) map[string]string {
	data := make(map[string]string)
	for _, re := range strings.Split(raw, ",") {
		env := strings.Split(re, "=")
		data[env[0]] = env[1]
	}
	data["PushgatewayEndpoint"] = types.PushgatewayEndpoint
	return data
}

func megeLabel(data map[string]string) string {
	var str string
	for k, v := range data {
		str += k + "=" + v + ","
	}

	return strings.TrimSuffix(str, ",")
}
