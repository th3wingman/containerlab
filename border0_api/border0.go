// Copyright 2020 Nokia
// Licensed under the BSD 3-Clause License.
// SPDX-License-Identifier: BSD-3-Clause

package border0_api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/srl-labs/containerlab/nodes"
	"github.com/srl-labs/containerlab/utils"
	"gopkg.in/yaml.v2"
)

const (
	apiUrl                       = "https://api.border0.com/api/v1"
	ENV_NAME_BORDER0_ADMIN_TOKEN = "BORDER0_ADMIN_TOKEN"
	ENV_NAME_BORDER0_API         = "BORDER0_API"
)

var supportedSockTypes = []string{"ssh", "tls", "http", "https"}

// to avoid multiple token lookups etc. we'll cache the token.
var tokenCache = ""

// Login performs a login to border0.com and stores the retrieved the access-token in the cwd.
func Login(ctx context.Context, email, password string) error {
	// if password not set read from terminal
	if password == "" {
		var err error
		password, err = utils.ReadPasswordFromTerminal()
		if err != nil {
			return err
		}
	}
	// prepare a LoginRequest
	loginReq := &LoginRequest{
		Email:    email,
		Password: password,
	}
	// init a LoginResponse
	loginResp := &LoginResponse{}

	// execute the request
	err := Request(ctx, http.MethodPost, "login", loginResp, loginReq, false)
	if err != nil {
		return err
	}

	err = writeToken(loginResp.Token)
	if err != nil {
		return err
	}
	return nil
}

func getApiUrl() string {
	if os.Getenv(ENV_NAME_BORDER0_API) != "" {
		return os.Getenv(ENV_NAME_BORDER0_API)
	} else {
		return apiUrl
	}
}

// getToken retrieved the border0 access-token as a string.
func getToken() (string, error) {
	// return the cached token
	if tokenCache != "" {
		return tokenCache, nil
	}
	tokenData := ""
	// Environement variable provided token
	if os.Getenv(ENV_NAME_BORDER0_ADMIN_TOKEN) != "" {
		tokenData = os.Getenv(ENV_NAME_BORDER0_ADMIN_TOKEN)
		log.Debugf("sourcing Border0.com token from %q environment variable", tokenData)
	} else {
		// resolve the token file
		tokenFile, err := tokenfile()
		if err != nil {
			return "", err
		}
		// read in the token from the resolved file
		tokenBytes, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", err
		}
		tokenData = string(tokenBytes)
	}

	// also store the token is cache
	tokenCache := strings.TrimSpace(tokenData)

	return tokenCache, nil
}

// checkPoliciesExist given a Map of policy names, will figure out if these policies already on the border0 side.
func checkPoliciesExist(ctx context.Context, policyNames map[string]struct{}) error {
	// retrieve the existing policies
	policies, err := GetExistingPolicies(ctx)
	if err != nil {
		return err
	}
	// keep track of the non-found policy names
	notFound := []string{}
OUTER:
	// iterate over the given policy names
	for name := range policyNames {
		// iterate over the retrieved policies
		for _, policy := range policies {
			if name == policy.Name {
				// if a policy with the name is found continue with next
				continue OUTER
			}
		}
		// if not found add to the list
		notFound = append(notFound, name)
	}
	// if we have non found poolicies, raise error
	if len(notFound) > 0 {
		return fmt.Errorf("border0.com policies %q referenced but they don't exist", strings.Join(notFound, ", "))
	}
	// if we've found all items return nil
	return nil
}

// tokenfile retrieve the location of the token file. It can reside in different locations and the most valid will be resolved here
// already validates that the file exists.
func tokenfile() (string, error) {
	tokenFile := ""
	// iterate over the possible border0.com token file locations
	for i := 0; i <= 1; i++ {
		switch i {
		case 0:
			// Current Working Directory location
			cwd, _ := os.Getwd()
			tokenFile = fmt.Sprintf("%s/.border0_token", cwd)
		case 1:
			// Home directory location
			tokenFile = fmt.Sprintf("%s/.border0/token", os.Getenv("HOME"))
		}
		// if file exists return
		if utils.FileExists(tokenFile) {
			return tokenFile, nil
		}
	}
	// no valid file found, return error
	return "", fmt.Errorf("no access-token found, please login to border0.com first e.g use `containerlab tools border0 login --email <BORDER0-USER-MAIL-ADDRESS>`")
}

// cwdTokenFilePath get the abspath of the token file in the current working directory.
func cwdTokenFilePath() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return filepath.Join(cwd, ".border0_token")
}

// GetExistingPolicies retrieved the existing policies from border0.com.
func GetExistingPolicies(ctx context.Context) ([]Policy, error) {
	var policies []Policy
	err := Request(ctx, http.MethodGet, "policies", &policies, nil, true)
	if err != nil {
		return nil, err
	}
	return policies, nil
}

// Request is the helper function that handels the http requests, as well as the marshalling of request structs and unmarshalling of responses.
func Request(ctx context.Context, method string, url string, targetStruct interface{}, data interface{}, requireAccessToken bool) error {
	jv, _ := json.Marshal(data)
	body := bytes.NewBuffer(jv)

	req, _ := http.NewRequestWithContext(ctx, method, fmt.Sprintf("%s/%s", getApiUrl(), url), body)

	token := ""
	// try to find the token in the environment
	if requireAccessToken {
		var err error
		token, err = getToken()
		if err != nil {
			return err
		}
	}
	if token != "" {
		token = strings.TrimSpace(token)
		req.Header.Add("x-access-token", token)
	}
	req.Header.Add("x-client-requested-with", "containerlab")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		return fmt.Errorf("401 Unauthorized, maybe the token expired")
	}

	if resp.StatusCode == 429 {
		responseData, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("rate limit error: %v", string(responseData))
	}

	if resp.StatusCode == 404 {
		return fmt.Errorf("404 NotFound")
	}

	if resp.StatusCode < 200 || resp.StatusCode > 204 {
		var errorMessage ErrorMessage
		json.NewDecoder(resp.Body).Decode(&errorMessage)

		return fmt.Errorf("failed to create object (%d) %v", resp.StatusCode, errorMessage.ErrorMessage)
	}

	if resp.StatusCode == 204 {
		return nil
	}

	err = json.NewDecoder(resp.Body).Decode(targetStruct)
	if err != nil {
		return fmt.Errorf("error decoding body: %w", err)
	}

	return nil
}

// RefreshLogin checking the validity of the login token as well as it.
func RefreshLogin(ctx context.Context) error {
	t := &LoginRefreshResponse{}
	log.Debug("Validating and refreshing border0.com token")
	err := Request(ctx, http.MethodPost, "login/refresh", t, nil, true)
	if err != nil {
		return err
	}
	err = writeToken(t.Token)
	if err != nil {
		return err
	}
	return nil
}

// writeToken writes the given token to a file.
func writeToken(token string) error {
	absPathToken := cwdTokenFilePath()

	err := os.WriteFile(absPathToken, []byte(token), 0600)
	if err != nil {
		return fmt.Errorf("failed to write border0.com token file as %s: %v",
			absPathToken, err)
	}
	log.Debugf("Saved border0.com token to %s", absPathToken)
	// also update the token cache
	tokenCache = token
	return nil
}

// CreateBorder0Config inspects the `publish` section of the nodes configuration and builds a configuration for
// the border0.com cli clients "Static Sockets plugin" [https://docs.border0.com/docs/static-sockets-plugin]
func CreateBorder0Config(ctx context.Context, nodesMap map[string]nodes.Node, labname string) (string, error) {
	log.Debug("Creating the border0.com configuration")
	// acquire token
	border0Token, err := getToken()
	if err != nil {
		return "", err
	}
	// init config struct
	yamlConfig := &StaticSocketsConfig{
		Connector: &configConnector{
			Name: labname,
		},
		Credentials: &configCredentials{
			Token: border0Token,
		},
		Sockets: []map[string]*configSocket{},
	}

	// helper map to collect all the referenced policies for later existence check
	policyNames := map[string]struct{}{}

	// iterate over structs to generate socket configs
	for _, n := range nodesMap {
		// grab short name and kind
		nodeShortName := n.Config().ShortName
		nodeKind := n.Config().Kind
		// loop thru the publish section of the node config
		for _, socket := range n.Config().Publish {
			// Parse the socket config
			sockConfig, err := ParseSocketCfg(socket, n.Config().LongName, nodeKind)
			if err != nil {
				return "", err
			}

			// add the referenced policies to the map for later checking
			for _, policy := range sockConfig.Policies {
				policyNames[policy] = struct{}{}
			}

			// check if socket is border0
			if nodeShortName == "border0" {
				// set the socketname for border0 bastion
				socketName := fmt.Sprintf("%s-bastion", n.Config().LongName)
				yamlConfig.Sockets = append(yamlConfig.Sockets, map[string]*configSocket{
					socketName: sockConfig,
				})
				// set the connector metadata:
				yamlConfig.Connector.ProviderType = "aws"
				yamlConfig.Connector.ProviderEnvironment = "containerlab"
				yamlConfig.Connector.ProviderRegion = labname

			} else {
				// determine the socketname
				socketName := fmt.Sprintf("%s-%s-%d", n.Config().LongName, sockConfig.Type, sockConfig.Port)
				yamlConfig.Sockets = append(yamlConfig.Sockets, map[string]*configSocket{
					socketName: sockConfig,
				})
			}
		}
	}

	// check for the existence of referenced policies
	err = checkPoliciesExist(ctx, policyNames)
	if err != nil {
		return "", err
	}

	// marshall the config into a []byte
	bconfig, err := yaml.Marshal(yamlConfig)
	if err != nil {
		return "", err
	}

	// return yaml based string configuration
	return string(bconfig), nil
}

// ParseSocketCfg parses the nodes publish configuration string and returns resulting *configSocket.
func ParseSocketCfg(s, host string, kind string) (*configSocket, error) {
	result := &configSocket{}
	// split the socket definition string
	split := strings.Split(s, "/")
	if len(split) > 3 {
		return result, fmt.Errorf("wrong publish section %q. should be <type>/<port-number>[/<policyname>,] i.e. tls/22, tls/22/mypolicy1 or tls/22/mypolicy1,myotherpolicy", s)
	}

	// process SocketType
	if err := checkSockType(split[0]); err != nil {
		return result, err
	}
	result.Type = split[0]
	// process port, check if split[1] is sshserver and type is ssh
	if split[0] == "ssh" && split[1] == "server" {
		result.SSHServer = true
	} else {
		// process host
		result.Host = host

		// we are parsing out the port and making it an int
		p, err := strconv.Atoi(split[1]) // port
		if err != nil {
			return result, err
		}
		if err := checkSockPort(p); err != nil {
			return result, err
		}
		result.Port = p
	}

	// process policy
	result.Policies = []string{}
	if len(split) == 3 {
		// split the possible multiple policies
		splitPolicy := strings.Split(split[2], ",")
		// add all the mentioned policies to the result
		for _, x := range splitPolicy {
			// trim whitespaces and add the result
			result.Policies = append(result.Policies, strings.TrimSpace(x))
		}
	}
	// setting the Default credentials based on the kind of the node
	if result.Type == "ssh" {
		// set defauilt creds for srl
		if kind == "srl" {
			result.UpstreamUsername = "admin"
			result.UpstreamPassword = "NokiaSrl1!"
		}
		// set defauilt creds for cvx
		if kind == "cvx" {
			result.UpstreamUsername = "root"
			result.UpstreamPassword = "root"
		}
		// set defauilt creds for vr-ros
		if kind == "vr-ros" {
			result.UpstreamUsername = "admin"
			result.UpstreamPassword = "admin"
		}
		//set defauilt creds for vr-mikrotik_ros
		if kind == "vr-mikrotik_ros" {
			result.UpstreamUsername = "admin"
			result.UpstreamPassword = "admin"
		}

	}

	return result, nil

}

// checkSockType checks that the types of the socket definitions from the publish config section
// contains a valid value.
func checkSockType(t string) error {
	if _, ok := utils.StringInSlice(supportedSockTypes, t); !ok {
		return fmt.Errorf("border0.com does not support socket type %q. Supported types are %q",
			t, strings.Join(supportedSockTypes, "|"))
	}
	return nil
}

// checkSockPort checks that the port of the socket definitions from the publish config section
// is in the valid range of portnumbers.
func checkSockPort(p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("incorrect port number %v", p)
	}
	return nil
}
