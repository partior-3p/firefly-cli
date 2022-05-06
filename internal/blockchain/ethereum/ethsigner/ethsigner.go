// Copyright © 2022 Kaleido, Inc.
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

package ethsigner

import (
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/hyperledger/firefly-cli/internal/blockchain/ethereum"
	"github.com/hyperledger/firefly-cli/internal/constants"
	"github.com/hyperledger/firefly-cli/internal/docker"
	"github.com/hyperledger/firefly-cli/internal/log"
	"github.com/hyperledger/firefly-cli/pkg/types"
)

var ethsignerImage = "ghcr.io/hyperledger/firefly-signer:v0.9.1"
var gethImage = "ethereum/client-go:release-1.10"

const useJavaSigner = false // also need to change the image appropriately if you recompile to use the Java signer

type EthSignerProvider struct {
	Log     log.Logger
	Verbose bool
	Stack   *types.Stack
}

func (p *EthSignerProvider) WriteConfig(options *types.InitOptions, rpcURL string) error {

	// Write the password that will be used to encrypt the private key
	// TODO: Probably randomize this and make it differnet per member?
	initDir := filepath.Join(constants.StacksDir, p.Stack.Name, "init")
	blockchainDirectory := filepath.Join(initDir, "blockchain")
	if err := os.MkdirAll(blockchainDirectory, 0755); err != nil {
		return err
	}
	if err := ioutil.WriteFile(filepath.Join(initDir, "blockchain", "password"), []byte("correcthorsebatterystaple"), 0755); err != nil {
		return err
	}

	signerConfigPath := filepath.Join(initDir, "config", "ethsigner.yaml")
	if err := GenerateSignerConfig(options.ChainID, rpcURL).WriteConfig(signerConfigPath); err != nil {
		return nil
	}

	for _, member := range p.Stack.Members {
		account := member.Account.(*ethereum.Account)
		// Write the private key to disk for each member
		if err := p.writeAccountToDisk(p.Stack.InitDir, account.Address, account.PrivateKey); err != nil {
			return err
		}

		if err := p.writeTomlKeyFile(p.Stack.InitDir, account.Address); err != nil {
			return err
		}
	}

	return nil
}

func (p *EthSignerProvider) FirstTimeSetup() error {
	ethsignerVolumeName := fmt.Sprintf("%s_ethsigner", p.Stack.Name)
	blockchainDir := filepath.Join(p.Stack.RuntimeDir, "blockchain")
	contractsDir := filepath.Join(p.Stack.RuntimeDir, "contracts")

	if err := docker.CreateVolume(ethsignerVolumeName, p.Verbose); err != nil {
		return err
	}

	if err := os.MkdirAll(contractsDir, 0755); err != nil {
		return err
	}

	// Copy the signer config to the volume
	signerConfigPath := filepath.Join(p.Stack.StackDir, "runtime", "config", "ethsigner.yaml")
	signerConfigVolumeName := fmt.Sprintf("%s_ethsigner_config", p.Stack.Name)
	docker.CopyFileToVolume(signerConfigVolumeName, signerConfigPath, "firefly.ffsigner", p.Verbose)

	// Mount the directory containing all members' private keys and password, and import the accounts using the geth CLI
	// Note: This is needed because of licensing issues with the Go Ethereum library that could do this step
	for _, member := range p.Stack.Members {
		account := member.Account.(*ethereum.Account)
		if err := p.importAccountToEthsigner(account.Address); err != nil {
			return err
		}
	}

	// Copy the password (to be used for decrypting private keys)
	if err := docker.CopyFileToVolume(ethsignerVolumeName, path.Join(blockchainDir, "password"), "password", p.Verbose); err != nil {
		return err
	}

	return nil
}

func (p *EthSignerProvider) getCommand(rpcURL string) string {
	if !useJavaSigner {
		return ""
	}

	// The Java based signing runtime if swapped in, requires these command line parameters
	u, err := url.Parse(rpcURL)
	if err != nil || rpcURL == "" {
		panic(fmt.Errorf("RPC URL invalid '%s': %s", rpcURL, err))
	}
	ethsignerCommand := []string{}
	ethsignerCommand = append(ethsignerCommand, fmt.Sprintf(`--logging=DEBUG`))
	ethsignerCommand = append(ethsignerCommand, fmt.Sprintf(`--chain-id=%d`, p.Stack.ChainID()))
	ethsignerCommand = append(ethsignerCommand, fmt.Sprintf(`--downstream-http-host=%s`, u.Hostname()))
	port := u.Port()
	if u.Scheme == "https" {
		ethsignerCommand = append(ethsignerCommand, `--downstream-http-tls-enabled`)
		if port == "" {
			port = "443"
		}
	}
	if u.Path != "" && u.Path != "/" {
		ethsignerCommand = append(ethsignerCommand, fmt.Sprintf(`--downstream-http-path=%s`, u.Path))
	}
	ethsignerCommand = append(ethsignerCommand, fmt.Sprintf(`--downstream-http-port=%s`, port))
	ethsignerCommand = append(ethsignerCommand, `multikey-signer`)
	ethsignerCommand = append(ethsignerCommand, `--directory=/data/keystore`)
	return strings.Join(ethsignerCommand, " ")
}

func (p *EthSignerProvider) GetDockerServiceDefinition(rpcURL string) *docker.ServiceDefinition {
	addresses := ""
	for i, member := range p.Stack.Members {
		account := member.Account.(*ethereum.Account)
		addresses = addresses + account.Address
		if i+1 < len(p.Stack.Members) {
			addresses = addresses + ","
		}
	}

	return &docker.ServiceDefinition{
		ServiceName: "ethsigner",
		Service: &docker.Service{
			Image:         ethsignerImage,
			ContainerName: fmt.Sprintf("%s_ethsigner", p.Stack.Name),
			User:          "root",
			Command:       p.getCommand(rpcURL),
			Volumes: []string{
				"ethsigner:/data",
				fmt.Sprintf("ethsigner_config:/etc/firefly"),
			},
			Logging: docker.StandardLogOptions,
			HealthCheck: &docker.HealthCheck{
				Test: []string{
					"CMD",
					"curl",
					"-X", "POST",
					"-H", "Content-Type: application/json",
					"-d", `{"jsonrpc":"2.0","method":"net_version","params":[],"id":"1"}`,
					"-w", "%{http_code}",
					"-sS",
					"--fail",
					"http://localhost:8545/",
				},
				Interval: "15s", // 6000 requests in a day
				Retries:  60,
			},
			Ports: []string{fmt.Sprintf("%d:8545", p.Stack.ExposedBlockchainPort)},
		},
		VolumeNames: []string{
			"ethsigner",
			"ethsigner_config",
		},
	}
}

func (p *EthSignerProvider) CreateAccount(args []string) (interface{}, error) {
	address, privateKey := ethereum.GenerateAddressAndPrivateKey()

	if err := p.writeAccountToDisk(p.Stack.RuntimeDir, address, privateKey); err != nil {
		return nil, err
	}

	if err := p.writeTomlKeyFile(p.Stack.RuntimeDir, address); err != nil {
		return nil, err
	}

	if err := p.importAccountToEthsigner(address); err != nil {
		return nil, err
	}

	return map[string]string{
		"address":    address,
		"privateKey": privateKey,
	}, nil
}