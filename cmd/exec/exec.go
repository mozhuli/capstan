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

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ZJU-SEL/capstan/pkg/util"
	"github.com/golang/glog"
	"github.com/spf13/pflag"
	apismetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/tools/remotecommand"
)

var (
	kubeconfig = pflag.String("kubeconfig", "/etc/kubernetes/admin.conf", "path to kubernetes admin config file")
	podName    = pflag.StringP("pod", "p", "", "pod name to exec")
	namespace  = pflag.StringP("namespace", "n", "default", "namespace to exec")
	container  = pflag.StringP("container", "c", "", "container name to exec")
	command    = pflag.StringP("command", "m", "/bin/sh", "exec command")
	args       = pflag.StringP("args", "a", "", "arguments to exec")

	version = pflag.Bool("version", false, "Display version")
	// VERSION is the version of exec.
	VERSION = "1.0"
)

func main() {
	util.InitFlags()
	util.InitLogs()
	defer util.FlushLogs()

	// Print exec version
	if *version {
		fmt.Println(VERSION)
		os.Exit(0)
	}

	result, err := execCommand(*podName, *container, *namespace, *command)
	if err != nil {
		glog.Fatal(err)
	}
	fmt.Println(result)
}

func newStringReader(ss []string) io.Reader {
	formattedString := strings.Join(ss, "\n")
	reader := strings.NewReader(formattedString)
	return reader
}

func execCommand(podName, container, namespace, command string) (string, error) {
	// Create kubernetes client config. Use kubeconfig if given, otherwise assume in-cluster.
	config, err := util.NewClusterConfig(*kubeconfig)
	if err != nil {
		glog.Fatal(err)
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatal(err)
	}

	glog.V(3).Infof("Exec pod %q container %q in namespace %q with command %v", podName, container, namespace, command)
	var (
		execOut bytes.Buffer
		execErr bytes.Buffer
	)

	_, err = kubeClient.CoreV1().Pods(namespace).Get(podName, apismetav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("could not get pod info: %v", err)
	}

	req := kubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		Param("container", container).
		Param("command", command).
		Param("stdin", "true").
		Param("stdout", "true").
		Param("stderr", "true").
		Param("tty", "false")

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("failed to init executor: %v", err)
	}

	stdIn := newStringReader([]string{"-c", *args})

	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  stdIn,
		Stdout: &execOut,
		Stderr: &execErr,
		Tty:    false,
	})

	if err != nil {
		return "", fmt.Errorf("could not execute: %v", err)
	}

	if execErr.Len() > 0 {
		return "", fmt.Errorf("stderr: %v", execErr.String())
	}

	return execOut.String(), nil
}
