// Copyright © 2021 Kaleido, Inc.
//
// SPDX-License-Identifier: Apache-2.0
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

package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strings"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/hyperledger/firefly-cli/internal/log"
)

type (
	CtxIsLogCmdKey       struct{}
	CtxComposeVersionKey struct{}
	DockerComposeVersion int
)

const (
	None DockerComposeVersion = iota
	ComposeV1
	ComposeV2
)

func CreateVolume(ctx context.Context, volumeName string) error {
	return RunDockerCommand(ctx, ".", "volume", "create", volumeName)
}

func CopyFileToVolume(ctx context.Context, volumeName string, sourcePath string, destPath string) error {
	fileName := path.Base(sourcePath)
	return RunDockerCommand(ctx, ".", "run", "--rm", "-v", fmt.Sprintf("%s:/source/%s", sourcePath, fileName), "-v", fmt.Sprintf("%s:/dest", volumeName), "alpine", "cp", "-R", path.Join("/", "source", fileName), path.Join("/", "dest", destPath))
}

func MkdirInVolume(ctx context.Context, volumeName string, directory string) error {
	return RunDockerCommand(ctx, ".", "run", "--rm", "-v", fmt.Sprintf("%s:/dest", volumeName), "alpine", "mkdir", "-p", path.Join("/", "dest", directory))
}

func RemoveVolume(ctx context.Context, volumeName string) error {
	return RunDockerCommand(ctx, ".", "volume", "remove", volumeName)
}

func CopyFromContainer(ctx context.Context, containerName string, sourcePath string, destPath string) error {
	if err := RunDockerCommand(ctx, ".", "cp", containerName+":"+sourcePath, destPath); err != nil {
		return err
	}
	return nil
}

func RunDockerCommandRetry(ctx context.Context, workingDir string, retries int, command ...string) error {
	attempt := 0
	for {
		err := RunDockerCommand(ctx, workingDir, command...)
		if err != nil && attempt < retries {
			attempt++
			continue
		} else if err != nil {
			return err
		}
		break
	}
	return nil
}

func RunDockerCommand(ctx context.Context, workingDir string, command ...string) error {
	dockerCmd := exec.Command("docker", command...)
	dockerCmd.Dir = workingDir
	_, err := runCommand(ctx, dockerCmd)
	return err
}

func RunDockerComposeCommand(ctx context.Context, workingDir string, command ...string) error {
	switch ctx.Value(CtxComposeVersionKey{}) {
	case ComposeV1:
		dockerCmd := exec.Command("docker-compose", command...)
		dockerCmd.Dir = workingDir
		_, err := runCommand(ctx, dockerCmd)
		return err
	case ComposeV2:
		dockerCmd := exec.Command("docker", append([]string{"compose"}, command...)...)
		dockerCmd.Dir = workingDir
		_, err := runCommand(ctx, dockerCmd)
		return err
	default:
		return fmt.Errorf("No version for docker-compose has been detected.")
	}
}

func RunDockerCommandBuffered(ctx context.Context, workingDir string, command ...string) (string, error) {
	dockerCmd := exec.Command("docker", command...)
	dockerCmd.Dir = workingDir
	return runCommand(ctx, dockerCmd)
}

func runCommand(ctx context.Context, cmd *exec.Cmd) (string, error) {
	verbose := log.VerbosityFromContext(ctx)
	isLogCmd, _ := ctx.Value(CtxIsLogCmdKey{}).(bool)
	if verbose {
		fmt.Println(cmd.String())
	}
	outputBuff := strings.Builder{}
	stdoutChan := make(chan string)
	stderrChan := make(chan string)
	errChan := make(chan error)
	go pipeCommand(cmd, stdoutChan, stderrChan, errChan)

outputCapture:
	for {
		select {
		case s, ok := <-stdoutChan:
			if isLogCmd || verbose {
				if !ok {
					break outputCapture
				}
				fmt.Print(s)
			}
			outputBuff.WriteString(s)
		case s, ok := <-stderrChan:
			if !ok {
				break outputCapture
			}
			if verbose {
				fmt.Print(s)
			}
			outputBuff.WriteString(s)
		case err := <-errChan:
			return "", err
		}
	}
	cmd.Wait()
	statusCode := cmd.ProcessState.ExitCode()
	if statusCode != 0 {
		return "", fmt.Errorf("%s [%d] %s", strings.Join(cmd.Args, " "), statusCode, outputBuff.String())
	}
	return outputBuff.String(), nil
}

func pipeCommand(cmd *exec.Cmd, stdoutChan chan string, stderrChan chan string, errChan chan error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		errChan <- err
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		errChan <- err
		return
	}
	cmd.Start()
	go readPipe(stdout, stdoutChan, errChan)
	go readPipe(stderr, stderrChan, errChan)
}

func readPipe(pipe io.ReadCloser, outputChan chan string, errChan chan error) {
	buf := bufio.NewReader(pipe)
	for {
		line, err := buf.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				close(outputChan)
				return
			} else {
				errChan <- err
				close(outputChan)
				return
			}
		} else {
			outputChan <- line
		}
	}
}

func GetImageConfig(image string) (map[string]interface{}, error) {
	b, err := crane.Config(image)
	if err != nil {
		return nil, err
	}
	var jsonMap map[string]interface{}
	err = json.Unmarshal(b, &jsonMap)
	if err != nil {
		return nil, err
	}
	return jsonMap, nil
}

func GetImageLabel(image, label string) (string, error) {
	config, err := GetImageConfig(image)
	if err != nil {
		return "", err
	}
	c, ok := config["config"]
	if !ok {
		return "", nil
	}
	labels, ok := c.(map[string]interface{})["Labels"]
	if !ok {
		return "", nil
	}
	val, ok := labels.(map[string]interface{})[label]
	if !ok {
		return "", nil
	}
	return val.(string), nil
}

func GetImageDigest(image string) (string, error) {
	return crane.Digest(image)
}
