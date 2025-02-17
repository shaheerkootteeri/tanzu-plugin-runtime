// Copyright 2023 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v3"

	"github.com/vmware-tanzu/tanzu-plugin-runtime/config/internal/kubeconfig"
)

const (
	fakePluginScriptFmtString string = `#!/bin/bash
# Fake tanzu core binary

# fake command that simulates a context lcm operation
context() {
	if [ "%s" -eq "0" ]; then
		# regular output to stderr
		>&2 echo "$@ succeeded"
	else
		# error to stderr
		>&2 echo "$@ failed"
	fi

	exit %s
}

# fake alternate command to use
newcommand() {
	if [ "%s" -eq "0" ]; then
		# regular output to stdout
		echo "$@ succeeded"
	else
		# error to stderr
		>&2 echo "$@ failed"
	fi

	exit %s
}

case "$1" in
    # simulate returning an alternative set of args to invoke with, which
    # translates to running the command 'newcommand'
    %s) shift && shift && echo "newcommand $@";;
    newcommand)   $1 "$@";;
    context)   $1 "$@";;
    *) cat << EOF
Tanzu Core CLI Fake

Usage:
  tanzu [command]

Available Commands:
  update          fake command
  newcommand      fake new command
  _custom_command provide alternate command to invoke, if available
EOF
       exit 1
       ;;
esac
`
)

func cleanupTestingDir(t *testing.T) {
	p, err := LocalDir()
	assert.NoError(t, err)
	err = os.RemoveAll(p)
	assert.NoError(t, err)
}

func readOutput(t *testing.T, r io.Reader, c chan<- []byte) {
	data, err := io.ReadAll(r)
	if err != nil {
		t.Error(err)
	}
	c <- data
}

func TestGetKubeconfigForContext(t *testing.T) {
	err := setupForGetContext()
	assert.NoError(t, err)

	testKubeconfiFilePath := "../fakes/config/kubeconfig-1.yaml"
	kubeconfigFilePath, err := os.CreateTemp("", "config")
	assert.NoError(t, err)
	err = copyFile(testKubeconfiFilePath, kubeconfigFilePath.Name())
	assert.NoError(t, err)

	defer func() {
		cleanupTestingDir(t)
		_ = os.RemoveAll(kubeconfigFilePath.Name())
	}()

	c, err := GetContext("test-tanzu")
	assert.NoError(t, err)
	c.ClusterOpts.Path = kubeconfigFilePath.Name()
	c.ClusterOpts.Context = "tanzu-cli-mytanzu"
	err = SetContext(c, false)
	assert.NoError(t, err)

	// Test getting the kubeconfig for an arbitrary Tanzu resource
	kubeconfigBytes, err := GetKubeconfigForContext(c.Name, "project1", "space1")
	assert.NoError(t, err)
	c, err = GetContext("test-tanzu")
	assert.NoError(t, err)
	var kc kubeconfig.Config
	err = yaml.Unmarshal(kubeconfigBytes, &kc)
	assert.NoError(t, err)
	cluster := kubeconfig.GetCluster(&kc, "tanzu-cli-mytanzu/current")
	assert.Equal(t, cluster.Cluster.Server, c.ClusterOpts.Endpoint+"/project/project1/space/space1")

	// Test getting the kubeconfig for an arbitrary Tanzu resource
	kubeconfigBytes, err = GetKubeconfigForContext(c.Name, "project2", "")
	assert.NoError(t, err)
	c, err = GetContext("test-tanzu")
	assert.NoError(t, err)
	err = yaml.Unmarshal(kubeconfigBytes, &kc)
	assert.NoError(t, err)
	cluster = kubeconfig.GetCluster(&kc, "tanzu-cli-mytanzu/current")
	assert.Equal(t, cluster.Cluster.Server, c.ClusterOpts.Endpoint+"/project/project2")

	// Test getting the kubeconfig for an arbitrary Tanzu resource for non Tanzu context
	nonTanzuCtx, err := GetContext("test-mc")
	assert.NoError(t, err)
	_, err = GetKubeconfigForContext(nonTanzuCtx.Name, "project2", "")
	assert.Error(t, err)
	assert.ErrorContains(t, err, "context must be of type: tanzu")
}

func TestGetTanzuContextActiveResource(t *testing.T) {
	err := setupForGetContext()
	assert.NoError(t, err)

	defer cleanupTestingDir(t)

	c, err := GetContext("test-tanzu")
	assert.NoError(t, err)

	// Test getting the Tanzu active resource of a non-existent context
	_, err = GetTanzuContextActiveResource("non-existent-context")
	assert.Error(t, err)
	assert.ErrorContains(t, err, "context non-existent-context not found")

	// Test getting the Tanzu active resource of a context that is not Tanzu context
	_, err = GetTanzuContextActiveResource("test-mc")
	assert.Error(t, err)
	assert.ErrorContains(t, err, "context must be of type: tanzu")

	// Test getting the Tanzu active resource of a context with active resource as Org only
	activeResources, err := GetTanzuContextActiveResource("test-tanzu")
	assert.NoError(t, err)
	assert.Equal(t, activeResources.OrgID, "fake-org-id")
	assert.Empty(t, activeResources.ProjectName)
	assert.Empty(t, activeResources.SpaceName)

	// Test getting the Tanzu active resource of a context with active resource as space
	c.AdditionalMetadata[ProjectNameKey] = "fake-project"
	c.AdditionalMetadata[SpaceNameKey] = "fake-space"
	err = SetContext(c, false)
	assert.NoError(t, err)
	activeResources, err = GetTanzuContextActiveResource("test-tanzu")
	assert.NoError(t, err)
	assert.Equal(t, activeResources.OrgID, "fake-org-id")
	assert.Equal(t, activeResources.ProjectName, "fake-project")
	assert.Equal(t, activeResources.SpaceName, "fake-space")

	// If context activeMetadata is not set(error case)
	c.AdditionalMetadata = nil
	err = SetContext(c, false)
	assert.NoError(t, err)
	_, err = GetTanzuContextActiveResource("test-tanzu")
	assert.Error(t, err)
	assert.ErrorContains(t, err, "context is missing the Tanzu metadata")
}

func setupFakeCLI(dir string, exitStatus string, newCommandExitStatus string, enableCustomCommand bool) (string, error) {
	filePath := filepath.Join(dir, "tanzu")

	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0755)
	if err != nil {
		return "", err
	}
	defer f.Close()

	fakeCustomCommandName := "unused_command"
	// when enabled, the fake CLI script generated will be capable of
	// returning an alternate set of args for a provided set of args
	if enableCustomCommand {
		fakeCustomCommandName = customCommandName
	}

	fmt.Fprintf(f, fakePluginScriptFmtString, exitStatus, exitStatus, newCommandExitStatus, newCommandExitStatus, fakeCustomCommandName)

	return filePath, nil
}

func TestSetTanzuContextActiveResource(t *testing.T) {
	tests := []struct {
		test                 string
		exitStatus           string
		newCommandExitStatus string
		expectedOutput       string
		expectedFailure      bool
		enableCustomCommand  bool
	}{
		{
			test:            "with no alternate command and Tanzu active resource update successfully",
			exitStatus:      "0",
			expectedOutput:  "context update tanzu-active-resource test-context --project projectA --space spaceA succeeded\n",
			expectedFailure: false,
		},
		{
			test:            "with no alternate command and Tanzu active resource update unsuccessfully",
			exitStatus:      "1",
			expectedOutput:  "context update tanzu-active-resource test-context --project projectA --space spaceA failed\n",
			expectedFailure: true,
		},
		{
			test:                 "with alternate command and Tanzu active resource update successfully",
			newCommandExitStatus: "0",
			expectedOutput:       "newcommand update tanzu-active-resource test-context --project projectA --space spaceA succeeded\n",
			expectedFailure:      false,
			enableCustomCommand:  true,
		},
		{
			test:                 "with alternate command and Tanzu active resource update unsuccessfully",
			newCommandExitStatus: "1",
			expectedOutput:       "newcommand update tanzu-active-resource test-context --project projectA --space spaceA failed\n",
			expectedFailure:      true,
			enableCustomCommand:  true,
		},
	}

	for _, spec := range tests {
		dir, err := os.MkdirTemp("", "tanzu-set-tanzu-active-resource-api")
		assert.Nil(t, err)
		defer os.RemoveAll(dir)
		t.Run(spec.test, func(t *testing.T) {
			assert := assert.New(t)

			// Set up stdout and stderr for our test
			r, w, err := os.Pipe()
			if err != nil {
				t.Error(err)
			}
			c := make(chan []byte)
			go readOutput(t, r, c)
			stdout := os.Stdout
			stderr := os.Stderr
			defer func() {
				os.Stdout = stdout
				os.Stderr = stderr
			}()
			os.Stdout = w
			os.Stderr = w

			cliPath, err := setupFakeCLI(dir, spec.exitStatus, spec.newCommandExitStatus, spec.enableCustomCommand)
			assert.Nil(err)
			os.Setenv("TANZU_BIN", cliPath)

			// Test-1:
			// - verify correct string gets printed to default stdout and stderr
			err = SetTanzuContextActiveResource("test-context", "projectA", "spaceA")
			w.Close()
			stdoutRecieved := <-c

			if spec.expectedFailure {
				assert.NotNil(err)
			} else {
				assert.Nil(err)
			}

			assert.Equal(spec.expectedOutput, string(stdoutRecieved), "incorrect combinedOutput result")

			// Test-2: when external stdout and stderr are provided with WithStdout, WithStderr options,
			// verify correct string gets printed to provided custom stdout/stderr
			var combinedOutputBuff bytes.Buffer
			err = SetTanzuContextActiveResource("test-context", "projectA", "spaceA", WithOutputWriter(&combinedOutputBuff), WithErrorWriter(&combinedOutputBuff))
			if spec.expectedFailure {
				assert.NotNil(err)
			} else {
				assert.Nil(err)
			}
			assert.Equal(spec.expectedOutput, combinedOutputBuff.String(), "incorrect combinedOutputBuff result")

			os.Unsetenv("TANZU_BIN")
		})
	}
}
