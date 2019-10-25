// Copyright © 2019 The Tekton Authors.
//
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

package convert

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tektoncd/cli/pkg/cli"
	"github.com/tektoncd/cli/pkg/flags"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	buildv1 "github.com/openshift/api/build/v1"
)

var (
	errNoBuildConfig      = errors.New("missing buildconfig name")
	errInvalidBuildConfig = "buildconfig name %s does not exist in namespace %s"
)

type startOptions struct {
	cliparams cli.Params
	stream    *cli.Stream
}

type resourceOptionsFilter struct {
	git         []string
	image       []string
	cluster     []string
	storage     []string
	pullRequest []string
}

// NameArg validates that the first argument is a valid pipeline name
func NameArg(args []string, p cli.Params) error {
	if len(args) == 0 {
		return errNoBuildConfig
	}

	c, err := p.Clients()
	if err != nil {
		return err
	}

	name, ns := args[0], p.Namespace()
	_, err = c.OpenShiftBuild.BuildConfigs(ns).Get(name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf(errInvalidBuildConfig, name, ns)
	}

	return nil
}

func bcCommand(p cli.Params) *cobra.Command {
	opt := startOptions{
		cliparams: p,
	}

	c := &cobra.Command{
		Use:     "buildconfig [RESOURCES...] [PARAMS...]",
		Aliases: []string{"bc"},
		Short:   "Convert buildconfigs to tasks",
		Annotations: map[string]string{
			"commandType": "main",
		},
		Example: `
# convert buildconfig foo to a task named "foo" and taskrun spec whose name starts with "foo"
tkn convert buildconfig foo 

For params value, if you want to provide multiple values, provide them comma separated
like cat,foo,bar
`,
		SilenceUsage: true,
		Args: func(cmd *cobra.Command, args []string) error {
			if err := flags.InitParams(p, cmd); err != nil {
				return err
			}
			return NameArg(args, p)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			opt.stream = &cli.Stream{
				Out: cmd.OutOrStdout(),
				Err: cmd.OutOrStderr(),
			}
			return opt.run(args[0])
		},
	}

	//flags.AddShellCompletion(c.Flags().Lookup("serviceaccount"), "__kubectl_get_serviceaccount")
	//flags.AddShellCompletion(c.Flags().Lookup("task-serviceaccount"), "__kubectl_get_serviceaccount")

	_ = c.MarkZshCompPositionalArgumentCustom(1, "__tkn_convert_buildconfig")

	return c
}

func (opt *startOptions) run(pName string) error {
	var bc *buildv1.BuildConfig
	var err error
	if bc, err = opt.getInput(pName); err != nil {
		return err
	}

	return opt.startPipeline(bc)
}

func (opt *startOptions) getInput(pname string) (*buildv1.BuildConfig, error) {
	cs, err := opt.cliparams.Clients()
	if err != nil {
		return nil, err
	}

	bc, err := cs.OpenShiftBuild.BuildConfigs(opt.cliparams.Namespace()).Get(pname, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(opt.stream.Err, "failed to get buildconfig %s from %s namespace \n", pname, opt.cliparams.Namespace())
		return nil, err
	}

	return bc, nil
}

func (opt *startOptions) startPipeline(bc *buildv1.BuildConfig) error {
	tName := bc.Name
	// buildName not super important here, since we are not actually going
	// to interact with the build apiserver endpoint or build controller
	buildName := tName + "_1"
	namespace := bc.Namespace

	request := constructBuildRequest(buildName, namespace)

	//fmt.Fprintf(opt.stream.Out, "OpenShift Build Request %#v\n\n", request)

	build, err := opt.constructBuildObject(bc, request, buildName, namespace)
	if err != nil {
		return err
	}
	//fmt.Fprintf(opt.stream.Out, "OpenShift Build %#v\n\n", build)

	pod, err := opt.constructBuildPod(build)
	if err != nil {
		return err
	}
	//fmt.Fprintf(opt.stream.Out, "OpenShift Build Pod %#v\n\n", pod)

	t := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    bc.Namespace,
			GenerateName: tName,
		},
		Spec: v1alpha1.TaskSpec{
		},
	}
	for _, ic := range pod.Spec.InitContainers {
		step := v1alpha1.Step{
			Container: ic,
		}
		t.Spec.Steps = append(t.Spec.Steps, step)
	}
	for _, c := range pod.Spec.Containers {
		step := v1alpha1.Step{
			Container: c,
		}
		t.Spec.Steps = append(t.Spec.Steps, step)
	}
	for _, v := range pod.Spec.Volumes {
		t.Spec.Volumes = append(t.Spec.Volumes, v)
	}
	tr := &v1alpha1.TaskRun{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    bc.Namespace,
			GenerateName: tName + "-run",
		},
		Spec: v1alpha1.TaskRunSpec{
			TaskRef: &v1alpha1.TaskRef{Name: tName},
		},
	}

	if tBytes, err := json.Marshal(t); err == nil {
		var prettyJSON bytes.Buffer
		if err = json.Indent(&prettyJSON, tBytes, "", "\t"); err == nil {
			str := string(prettyJSON.Bytes())
			fmt.Fprintf(opt.stream.Out, "Tekton task json %s\n\n", str)
		}
	}

	fmt.Fprintf(opt.stream.Out, "SA to pass in: %s\n\n", pod.Spec.ServiceAccountName)
	tr.Spec.ServiceAccountName = pod.Spec.ServiceAccountName
	tr.Spec.PodTemplate.NodeSelector = pod.Spec.NodeSelector
	tr.Spec.PodTemplate.Tolerations = pod.Spec.Tolerations
	tr.Spec.PodTemplate.Affinity = pod.Spec.Affinity
	tr.Spec.PodTemplate.SecurityContext = pod.Spec.SecurityContext
	tr.Spec.PodTemplate.RuntimeClassName = pod.Spec.RuntimeClassName
	tr.ObjectMeta.Labels = pod.ObjectMeta.Labels
	tr.ObjectMeta.Annotations = pod.ObjectMeta.Annotations

	if trBytes, err := json.Marshal(tr); err == nil {
		var prettyJSON bytes.Buffer
		if err = json.Indent(&prettyJSON, trBytes, "", "\t"); err == nil{
			str := string(prettyJSON.Bytes())
			fmt.Fprintf(opt.stream.Out, "Tekton task run json %s\n\n", str)
		}
	}

	//fmt.Fprintf(opt.stream.Out, "Tekton task %#v\n\n", t)

	//fmt.Fprintf(opt.stream.Out, "Tekton task run %#v\n\n", t)
	return nil

}
