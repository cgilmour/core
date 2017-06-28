// Copyright (c) 2015 Pani Networks
// All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package common

// Contains the implementation of HttpClient and related utilities.

import (
	"bytes"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	jwt "github.com/dgrijalva/jwt-go"
	"github.com/romana/core/common/log/trace"
	log "github.com/romana/rlog"

	"github.com/pborman/uuid"
)

// Rest Client for the Romana services. Incorporates facilities to deal with
// various REST requests.
type RestClient struct {
	callNum        uint64
	url            *url.URL
	client         *http.Client
	token          string
	config         *RestClientConfig
	mu             sync.Mutex
	lastStatusCode int
}

// RestClientConfig holds configuration for the Romana RESTful client.
type RestClientConfig struct {
	TimeoutMillis int64
	Retries       int
	RetryStrategy string
	Credential    *Credential
	TestMode      bool
	RootURL       string
}

// GetDefaultRestClientConfig gets a RestClientConfig with specified rootURL
// and other values set to their defaults, such as
// DefaultRestTimeout, DefaultRestRetries.
func GetDefaultRestClientConfig(rootURL string, cred *Credential) RestClientConfig {
	return RestClientConfig{TimeoutMillis: DefaultRestTimeout,
		Retries:    DefaultRestRetries,
		RootURL:    rootURL,
		Credential: cred,
	}
}

// GetRestClientConfig returns a RestClientConfig based on a ServiceConfig. That is,
// the information provided in the service configuration is used for the client
// configuration.
func GetRestClientConfig(config ServiceConfig, cred *Credential) RestClientConfig {
	retries := config.Common.Api.RestRetries
	if retries <= 0 {
		retries = DefaultRestRetries
	}
	return RestClientConfig{
		TimeoutMillis: getTimeoutMillis(config.Common),
		Retries:       retries,
		RootURL:       config.Common.Api.RootServiceUrl,
		TestMode:      config.Common.Api.RestTestMode,
		Credential:    cred,
	}
}

// NewRestClient creates a new Romana REST client. It provides convenience
// methods to make REST calls. When configured with a root URL pointing
// to Romana root service, it provides some common functionality useful
// for Romana services (such as ListHosts, GetServiceConfig, etc.)
// If the root URL does not point to the Romana service, the generic REST operations
// still work, but Romana-specific functionality does not.
func NewRestClient(config RestClientConfig) (*RestClient, error) {
	rc := &RestClient{client: &http.Client{}, config: &config}
	if config.RetryStrategy != RestRetryStrategyExponential && config.RetryStrategy != RestRetryStrategyFibonacci {
		rc.logf("Invalid retry strategy %s, defaulting to %s\n", config.RetryStrategy, RestRetryStrategyFibonacci)
		config.RetryStrategy = RestRetryStrategyFibonacci
	}
	timeoutMillis := config.TimeoutMillis
	if timeoutMillis <= 0 {
		//		rc.logf("Invalid timeout %d, defaulting to %d\n", timeoutMillis, DefaultRestTimeout)
		rc.client.Timeout = DefaultRestTimeout * time.Millisecond
	} else {
		timeoutStr := fmt.Sprintf("%dms", timeoutMillis)
		dur, _ := time.ParseDuration(timeoutStr)
		rc.logf("Setting timeout to %v\n", dur)
		rc.client.Timeout = dur
	}
	if config.Retries < 1 {
		//		rc.logf("Invalid retries %d, defaulting to %d\n", config.Retries, DefaultRestRetries)
		config.Retries = DefaultRestRetries
	}
	var myUrl string
	// Whether this is to be used in a Romana context or a generic
	// REST client. In Romana context, authentication will be used.
	var isRomana bool
	if config.RootURL == "" {
		isRomana = false
		// Default to some URL. This client would not be able to be used
		// for Romana-related service convenience methods, just as a generic
		// REST client.
		// If we keep this empty, NewUrl wouldn't work properly when
		// trying to resolve things.
		myUrl = "http://localhost"
	} else {
		isRomana = true
		u, err := url.Parse(config.RootURL)
		if err != nil {
			return nil, err
		}
		if !u.IsAbs() {
			return nil, NewError("Expected absolute URL for root, received %s", config.RootURL)
		}
		myUrl = config.RootURL

	}
	err := rc.NewUrl(myUrl)
	if err != nil {
		return nil, err
	}
	if isRomana {
		err := rc.Authenticate()
		if err != nil {
			return rc, err
		}
	}
	return rc, nil
}

func (rc *RestClient) log(arg interface{}) {
	rc.logf("%+v", arg)
}

// logf is same as log.Tracef but adds a prefix to the line
// with the ID of this RestClient instance and the number
// of the call.
func (rc *RestClient) logf(s string, args ...interface{}) {
	// TODO of course using GetCaller() here is
	s1 := fmt.Sprintf("RestClient.%p.%d: %s: %s\n", rc, rc.callNum, GetCaller2(2), s)
	log.Tracef(trace.Inside, s1, args...)
}

// NewUrl sets the client's new URL (yes, it mutates) to dest.
// If dest is a relative URL then it will be based
// on the previous value of the URL that the RestClient had.
func (rc *RestClient) NewUrl(dest string) error {
	return rc.modifyUrl(dest, nil)
}

// GetStatusCode returns status code of last executed request.
// As stated above, it is not recommended to share RestClient between
// goroutines. 0 is returned if no previous requests have been yet
// made, or if the most recent request resulted in some error that
// was not a 4xx or 5xx HTTP error.
func (rc *RestClient) GetStatusCode() int {
	return rc.lastStatusCode
}

// ListHost queries the Topology service in order to return a list of currently
// configured hosts in a Romana cluster.
func (rc *RestClient) ListHosts() ([]Host, error) {
	// Save the current state of things, so we can restore after call to root.
	savedUrl := rc.url
	// Restore this after we're done so we don't lose this
	defer func() {
		rc.url = savedUrl
	}()

	topoUrl, err := rc.GetServiceUrl("topology")
	if err != nil {
		return nil, err
	}
	topIndex := IndexResponse{}
	err = rc.Get(topoUrl, &topIndex)
	if err != nil {
		return nil, err
	}
	hostsRelURL := topIndex.Links.FindByRel("host-list")

	var hostList []Host
	err = rc.Get(hostsRelURL, &hostList)
	return hostList, err
}

// Find is a convenience function, which queries the appropriate service
// and retrieves one entity based on provided structure, and puts the results
// into the same structure. The provided argument, entity, should be a pointer
// to the desired structure, e.g., &common.Host{}.
func (rc *RestClient) Find(entity interface{}, flag FindFlag) error {
	structType := reflect.TypeOf(entity).Elem()
	if flag == FindAll {
		structType = structType.Elem()
	}

	entityName := structType.String()
	entityDottedNames := strings.Split(entityName, ".")
	if len(entityDottedNames) > 1 {
		entityName = entityDottedNames[1]
	}
	entityName = strings.ToLower(entityName)
	var serviceName string
	switch entityName {
	case "tenant":
		serviceName = "tenant"
	case "segment":
		serviceName = "tenant"
	case "host":
		serviceName = "topology"
	default:
		return NewError("Do not know where to find entity '%s'", entityName)
	}
	svcURL, err := rc.GetServiceUrl(serviceName)
	rc.logf("Attempting to get URL for service %s: %s (%+v)", serviceName, svcURL, err)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(svcURL, "/") {
		svcURL += "/"
	}
	svcURL += fmt.Sprintf("%s/%ss?", flag, entityName)
	queryString := ""

	structValue := reflect.ValueOf(entity).Elem()
	// Whether findAll is constrained -- the provided pointer is to a slice
	// that has an element with the filled in struct.
	var findAllConstrained bool
	if flag == FindAll {
		if structValue.Kind() != reflect.Slice && structValue.Kind() != reflect.Array {
			return NewError("Expected slice or array with FindAll, received %s", structValue.Kind())
		}
		// If we have an empty array, then we just want to do a FindAll. But if there is
		// any constituent in the array, then that element has constraints.
		if structValue.Len() > 0 {
			findAllConstrained = true
			structValue = structValue.Index(0)
		}
	}

	// Construct querystring only if flag is not FindAll or findAllConstrained is true --
	// see above.
	if flag != FindAll || findAllConstrained {
		for i := 0; i < structType.NumField(); i++ {
			structField := structType.Field(i)
			fieldTag := structField.Tag
			fieldName := structField.Name
			queryStringFieldName := strings.ToLower(fieldName)
			omitEmpty := false
			if fieldTag != "" {
				jTag := fieldTag.Get("json")
				if jTag != "" {
					jTagElts := strings.Split(jTag, ",")
					// This takes care of ",omitempty"
					if len(jTagElts) > 1 {
						queryStringFieldName = jTagElts[0]
						for _, jTag2 := range jTagElts {
							if jTag2 == "omitempty" {
								omitEmpty = true
								break
							} // if jTag2
						} // for / jTagElts
					} else {
						queryStringFieldName = jTag
					}
				} // if jTag
			} // if fieldTag
			//
			if !structValue.Field(i).CanInterface() {
				continue
			}
			fieldValue := structValue.Field(i).Interface()
			if omitEmpty && IsZeroValue(fieldValue) {
				continue
			}

			if queryString != "" {
				queryString += "&"
			}
			queryString += fmt.Sprintf("%s=%v", queryStringFieldName, fieldValue)
		}
	}
	url := svcURL + queryString
	rc.logf("Trying to find %s %s at %s, results go into %+v", entityName, flag, url, entity)
	return rc.Get(url, entity)
} // func

// GetServiceUrl is a convenience function, which, given the root
// service URL and name of desired service, returns the URL of that service.
func (rc *RestClient) GetServiceUrl(name string) (string, error) {
	// Save the current state of things, so we can restore after call to root.
	savedUrl := rc.url
	// Restore this after we're done so we don't lose this
	defer func() {
		rc.url = savedUrl
	}()
	resp := RootIndexResponse{}

	err := rc.Get(rc.config.RootURL, &resp)
	if err != nil {
		return ErrorNoValue, err
	}
	for i := range resp.Services {
		service := resp.Services[i]
		if service.Name == name {
			href := service.Links.FindByRel("service")
			if href != "" {
				// Now for a bit of a trick - this href could be relative...
				// Need to normalize.
				err = rc.NewUrl(href)
				if err != nil {
					return ErrorNoValue, err
				}
				return rc.url.String(), nil
			}
			return ErrorNoValue, errors.New(fmt.Sprintf("Cannot find service %s in response from %s: %+v", name, rc.config.RootURL, resp))
		}
	}
	return ErrorNoValue, errors.New(fmt.Sprintf("Cannot find service %s in response from %s: %+v", name, rc.config.RootURL, resp))
}

// modifyUrl sets the client's new URL to dest, possibly updating it with
// new values from the provided queryMod url.Values object.
// If dest is a relative URL then it will be based
// on the previous value of the URL that the RestClient had.
func (rc *RestClient) modifyUrl(dest string, queryMod url.Values) error {
	u, err := url.Parse(dest)
	if err != nil {
		return err
	}

	if rc.url == nil {
		if !u.IsAbs() {
			return errors.New("Expected absolute URL.")
		}
		rc.url = u
	} else {
		newUrl := rc.url.ResolveReference(u)
		//		rc.logf("Getting %s, resolved reference from %s to %s: %s\n", dest, rc.url, u, newUrl)
		rc.url = newUrl
	}

	if queryMod != nil {
		// If the queryMod (url.Values) object is provided, then the
		// query values in the current URL that match keys
		// from that queryMod object are replaced with those from queryMod.
		origUrl := rc.url
		origQuery := origUrl.Query()
		for k := range queryMod {
			origQuery[k] = queryMod[k]
		}
		dest := ""
		for k, v := range origQuery {
			for i := range v {
				if len(dest) > 0 {
					dest += "&"
				}
				dest += url.QueryEscape(k) + "=" + url.QueryEscape(v[i])
			}
		}
		dest = rc.url.Scheme + "://" + rc.url.Host + rc.url.Path + "?" + dest
		rc.url, _ = url.Parse(dest)
		//		rc.logf("Modified URL %s to %s (%v)\n", origUrl, rc.url, err)
	}

	return nil
}

// execMethod applies the specified method to the provided url (which is interpreted
// as relative or absolute).
// POST methods may not be idempotent, so for retry capability we will employ the following logic:
// 1. If the provided structure (data) already has a field "RequestToken", that means the service
//    is aware of this and is exposing RequestToken as a unique key to enable safe and idempotent
//    retries. The service would do that if it does not have another way to ensure uniqueness. An
//    example is IPAM service - a request for an IP address by itself does not necessarily have any
//    inherent properties that can ensure its uniqueness, unlike a request to, say, add a host (which
//    has an IP address). IPAM then uses RequestToken for that purpose.
// 2. If the provided structure does not have that field, and the query does not either, we are going
//    to generate a uuid and add it to the query as RequestToken=<UUID>. It will then be up to the service
//    to ensure idempotence or not.
func (rc *RestClient) execMethod(method string, dest string, data interface{}, result interface{}) error {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	// TODO check if token expired, if yes, reauthenticate... But this needs
	// more state here (knowledge of Root service by Rest client...)

	rc.callNum += 1
	rc.lastStatusCode = 0
	var queryMod url.Values
	queryMod = nil
	if method == "POST" && rc.config != nil && !rc.config.TestMode {
		var token string
		if data != nil {
			// If the provided struct has the RequestToken field,
			// we don't need to create a query parameter.
			v := reflect.Indirect(reflect.ValueOf(data))
			if !v.FieldByName(RequestTokenQueryParameter).IsValid() {
				queryParam := rc.url.Query().Get(RequestTokenQueryParameter)
				if queryParam == "" {
					queryMod = make(url.Values)
					token = uuid.New()
					rc.logf("Adding token to POST request: %s\n", token)
					queryMod[RequestTokenQueryParameter] = []string{token}
				}
			}
		}
	}
	err := rc.modifyUrl(dest, queryMod)

	//	rc.logf("RestClient: Set rc.url to %s\n", rc.url)
	if err != nil {
		return err
	}

	//	rc.logf("Scheme is %s, method is %s, test mode: %t", rc.url.Scheme, method, rc.config.TestMode)
	if rc.url.Scheme == "file" && method == "POST" && rc.config.TestMode {
		rc.logf("Attempt to POST to a file URL %s, in test mode will just return OK", rc.url)
		return nil
	}

	var reqBodyReader *bytes.Reader
	var reqBody []byte
	if data != nil {
		reqBody, err = json.Marshal(data)
		//		rc.logf("RestClient.execMethod(): Marshaled %T %v to %s", data, data, string(reqBody))
		if err != nil {
			return err
		}

	}
	var body []byte
	// We allow also file scheme, for testing purposes.
	var resp *http.Response
	var sleepTime time.Duration
	var prevSleepTime time.Duration
	if rc.url.Scheme == "http" || rc.url.Scheme == "https" {
		for i := 0; i < rc.config.Retries; i++ {
			var req *http.Request
			if data == nil {
				req, err = http.NewRequest(method, rc.url.String(), nil)
			} else {
				reqBodyReader = bytes.NewReader(reqBody)
				req, err = http.NewRequest(method, rc.url.String(), reqBodyReader)
				log.Infof("RestClient.execMethod(): Calling %s %s with %d bytes\n", method, rc.url.String(), reqBodyReader.Len())
			}
			if err != nil {
				return err
			}
			if reqBodyReader != nil {
				req.Header.Set("content-type", "application/json")
			}
			req.Header.Set("accept", "application/json")
			if rc.token != "" {
				rc.logf("Setting token in request to %s: %s", rc.url, rc.token)
				req.Header.Set("Authorization", rc.token)
			}
			if i > 0 {
				switch rc.config.RetryStrategy {
				case RestRetryStrategyExponential:
					sleepTime, _ = time.ParseDuration(fmt.Sprintf("%dms", 100*int(math.Pow(2, (float64(i-1))))))
				default:
					// Fibonacci
					if sleepTime == 0 {
						sleepTime = 100 * time.Millisecond
					} else {
						incr := prevSleepTime
						prevSleepTime = sleepTime
						sleepTime += incr
					}
				}
				time.Sleep(sleepTime)
			}
			if data != nil {
				reqBodyReader = bytes.NewReader(reqBody)
				rc.logf("RestClient: Attempting %s %s with content length %d: %s", method, rc.url.String(), reqBodyReader.Len(), string(reqBody))
			}
			resp, err = rc.client.Do(req)

			if err != nil {
				if i == rc.config.Retries-1 {
					return err
				}
				rc.logf("Error on try %d: %v", i, err)
				continue
			} else {
				// If service unavailable we may still retry...
				if resp.StatusCode != http.StatusServiceUnavailable {
					break
				}
			}
		}

		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, err = ioutil.ReadAll(resp.Body)

	} else if rc.url.Scheme == "file" {
		resp = &http.Response{}
		resp.StatusCode = http.StatusOK
		rc.logf("RestClient: Loading file %s, %s", rc.url.String(), rc.url.Path)
		body, err = ioutil.ReadFile(rc.url.Path)
		if err != nil {
			rc.logf("RestClient: Error loading file %s: %v", rc.url.Path, err)
			return err
		}
	} else {
		return errors.New(fmt.Sprintf("Unsupported scheme %s", rc.url.Scheme))
	}

	bodyStr := ""
	if body != nil {
		bodyStr = string(body)
	}
	errStr := ""
	if err != nil {
		errStr = fmt.Sprintf("ERROR: <%v>", err)
	}
	rc.logf("%s %s: %d\n%s", method, rc.url, resp.StatusCode, errStr)

	if err != nil {
		return err
	}

	var unmarshalBodyErr error

	//	TODO deal properly with 3xx
	//	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
	//		rc.logf("3xx: %d %v", resp.StatusCode, resp.Header)
	//	}
	//
	if result != nil {
		if body != nil {
			unmarshalBodyErr = json.Unmarshal(body, &result)
		}
	}

	rc.lastStatusCode = resp.StatusCode

	if resp.StatusCode >= 400 {
		// The body should be an HTTP error
		httpError := &HttpError{}
		unmarshalBodyErr = json.Unmarshal(body, httpError)
		httpError.StatusCode = resp.StatusCode
		if unmarshalBodyErr == nil {
			if resp.StatusCode == http.StatusConflict {
				// In case of conflict, we actually expect the object that caused
				// conflict to appear in details... So we want to marshal this back to JSON
				// and unmarshal into what we know should be...
				j, err := json.Marshal(httpError.Details)
				if err != nil {
					httpError.Details = errors.New(fmt.Sprintf("Error parsing '%v': %s", httpError.Details, err))
					return httpError
				}
				err = json.Unmarshal(j, &result)
				if err != nil {
					httpError.Details = errors.New(fmt.Sprintf("Error parsing '%s': %s", j, err))
					return httpError
				}
				httpError.Details = result
			}
			return *httpError
		}
		// Error unmarshaling body...
		httpError.Details = errors.New(fmt.Sprintf("Error parsing '%v': %s", bodyStr, err))
		return *httpError
	}
	// OK response...
	if unmarshalBodyErr != nil {
		return errors.New(fmt.Sprintf("Error %s (%T) when parsing %s", unmarshalBodyErr.Error(), err, body))
	}
	return nil
}

// Post applies POST method to the specified URL,
// putting the result into the provided interface
func (rc *RestClient) Post(url string, data interface{}, result interface{}) error {
	err := rc.execMethod("POST", url, data, result)
	return err
}

// Delete applies DELETE method to the specified URL,
// putting the result into the provided interface
func (rc *RestClient) Delete(url string, data interface{}, result interface{}) error {
	err := rc.execMethod("DELETE", url, data, result)
	return err
}

// Put applies PUT method to the specified URL,
// putting the result into the provided interface
func (rc *RestClient) Put(url string, data interface{}, result interface{}) error {
	err := rc.execMethod("PUT", url, data, result)
	return err
}

// Get applies GET method to the specified URL,
// putting the result into the provided interface
func (rc *RestClient) Get(url string, result interface{}) error {
	return rc.execMethod("GET", url, nil, result)
}

// Authenticate sends credential information to the Root's authentication
// URL and stores the token received.
func (rc *RestClient) Authenticate() error {
	if rc.config.Credential == nil || rc.config.Credential.Type == CredentialNone {
		return nil
	}
	rootIndexResponse := &RootIndexResponse{}
	if rc.config.RootURL == "" {
		return errors.New("RootURL not set")
	}
	err := rc.Get(rc.config.RootURL, rootIndexResponse)
	if err != nil {
		return err
	}
	authUrl := rootIndexResponse.Links.FindByRel("auth")
	rc.logf("Authenticating to %s as %s", authUrl, rc.config.Credential)
	tokenMsg := &AuthTokenMessage{}
	err = rc.Post(authUrl, rc.config.Credential, tokenMsg)
	if err != nil {
		return err
	}
	// TODO
	// It would be a good feature if the client itself could decrypt the token (which it can)
	// and, having figured out the expiration, re-auth when a request comes past
	// expiration.
	rc.logf("Received token %s", tokenMsg.Token)
	rc.token = tokenMsg.Token
	return nil
}

// GetPublicKey retrieves public key of root service used ot check
// auth tokens.
func (rc *RestClient) GetPublicKey() (*rsa.PublicKey, error) {
	rootIndexResponse := &RootIndexResponse{}
	if rc.config.RootURL == "" {
		return nil, errors.New("RestClient.GetPublicKey(): RootURL not set")
	}
	err := rc.Get(rc.config.RootURL, rootIndexResponse)
	if err != nil {
		return nil, err
	}

	relName := "publicKey"
	keyUrl := rootIndexResponse.Links.FindByRel(relName)
	if keyUrl == "" {
		return nil, errors.New(fmt.Sprintf("Could not find %s at %s (%+v)", relName, rc.config.RootURL, *rootIndexResponse))
	}
	rc.logf("GetPublicKey(): Found key url %s in %s from %s", keyUrl, rootIndexResponse, relName)
	var data []byte
	err = rc.Get(keyUrl, &data)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	key, err := jwt.ParseRSAPublicKeyFromPEM(data)
	if err != nil {
		return nil, err
	}
	return key, nil
}

// GetServiceConfig retrieves configuration
// for the given service from the root service.
func (rc *RestClient) GetServiceConfig(name string) (*ServiceConfig, error) {
	rootIndexResponse := &RootIndexResponse{}
	if rc.config.RootURL == "" {
		return nil, errors.New("RootURL not set")
	}
	err := rc.Get(rc.config.RootURL, rootIndexResponse)
	if err != nil {
		return nil, err
	}

	config := &ServiceConfig{}
	config.Common.Api = &Api{RootServiceUrl: rc.config.RootURL}
	relName := name + "-config"
	configUrl := rootIndexResponse.Links.FindByRel(relName)
	if configUrl == "" {
		return nil, errors.New(fmt.Sprintf("Could not find %s at %s", relName, rc.config.RootURL))
	}
	rc.logf("GetServiceConfig(): Found config url %s in %s from %s", configUrl, rootIndexResponse, relName)
	err = rc.Get(configUrl, config)
	if err != nil {
		return nil, err
	}
	// Save the credential from the client in the resulting service config --
	// if the resulting config is to be used in InitializeService(), it's useful;
	// otherwise, it will be ignored.
	config.Common.Credential = rc.config.Credential
	rc.logf("Saved from %v to %v", rc.config.Credential, config.Common.Credential)
	return config, nil
}
