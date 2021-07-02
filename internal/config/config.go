// Copyright (c) 2021 Doc.ai and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package aclconfig convert kubernetes config map to Go's map[string]string
package aclconfig

import (
	"context"
	"io/ioutil"
	"path/filepath"

	"github.com/edwarnicke/govpp/binapi/acl_types"
	"gopkg.in/yaml.v2"

	"github.com/networkservicemesh/sdk/pkg/tools/log"
)

type aclConfig struct {
	ACLRules map[string]acl_types.ACLRule
}

// GetACLRules reads config file with rules for acl filtering and return it as map
func GetACLRules(ctx context.Context, path string) (rules []acl_types.ACLRule) {
	var resultRules []acl_types.ACLRule
	logger := log.FromContext(ctx).WithField("acl", "config")

	raw, err := ioutil.ReadFile(filepath.Clean(path))
	if err != nil {
		logger.Errorf("Error reading config file: %v", err)
		return resultRules
	}
	logger.Infof("Read config file successfully")

	var rv aclConfig
	err = yaml.Unmarshal(raw, &rv)
	if err != nil {
		logger.Errorf("Error parsing config file: %v", err)
		return resultRules
	}
	logger.Infof("Parsed acl rules successfully")

	for _, v := range rv.ACLRules {
		resultRules = append(resultRules, v)
	}

	logger.Infof("Result rules:%v", resultRules)

	return resultRules
}
