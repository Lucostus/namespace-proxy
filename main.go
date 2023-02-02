package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"

	"github.com/gepaplexx/multena-proxy/pkg/labels_provider"
	"github.com/gepaplexx/multena-proxy/pkg/model"
	"github.com/gepaplexx/multena-proxy/pkg/utils"
	jwt "github.com/golang-jwt/jwt/v4"
	"github.com/google/gops/agent"
	"go.uber.org/zap"
)

func init() {
	utils.Logger.Info("Go Version", zap.String("version", runtime.Version()))
	utils.Logger.Info("Init Proxy")
	utils.Logger.Info("Set http client to ignore self signed certificates")
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	utils.InitJWKS()
	utils.InitKubeClient()
	utils.Logger.Info("Init Complete")
}

func main() {
	defer utils.Logger.Sync()
	utils.Logger.Info("Starting Agent")
	err := agent.Listen(agent.Options{
		ShutdownCleanup: true, // automatically closes on os.Interrupt
	})

	if nil != err {
		utils.LogPanic("Error while configuring agent", err)
	}

	utils.Logger.Info("Finished Starting Agent")
	utils.Logger.Info("Starting Proxy")
	// define origin server URLs
	originServerURL, err := url.Parse(os.Getenv("UPSTREAM_URL"))
	if err != nil {
		utils.LogPanic("originServerURL must be set", err)
	}

	utils.Logger.Debug("Upstream URL", zap.String("url", originServerURL.String()))
	originBypassServerURL, err := url.Parse(os.Getenv("UPSTREAM_BYPASS_URL"))
	if err != nil {
		utils.LogPanic("OriginBypassServerURL must be set", err)
	}

	utils.Logger.Debug("Bypass Upstream URL", zap.String("url", originBypassServerURL.String()))
	utils.Logger.Debug("Tenant Label", zap.String("label", os.Getenv("TENANT_LABEL")))
	tenantLabel := os.Getenv("TENANT_LABEL")

	reverseProxy := configureProxy(originBypassServerURL, tenantLabel, originServerURL)

	err = http.ListenAndServe(":8080", reverseProxy)
	if err != nil {
		utils.LogError("Error starting server", err)
		panic(err)
	}

}

func configureProxy(originBypassServerURL *url.URL, tenantLabel string, originServerURL *url.URL) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		utils.Logger.Info("Recived request", zap.String("request", fmt.Sprintf("%+v", req)))

		if req.Header.Get("Authorization") == "" {
			utils.Logger.Warn("No Authorization header found")
			rw.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(rw, "No Authorization header found")
			return
		}

		//parse jwt from request
		tokenString := string(req.Header.Get("Authorization"))[7:]
		var keycloakToken model.KeycloakToken
		token, err := jwt.ParseWithClaims(tokenString, &keycloakToken, utils.Jwks.Keyfunc)
		//if token invalid or expired, return 401

		utils.LogError("Token Parsing error", err)
		if !token.Valid {
			rw.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(rw, "error while parsing token")
			utils.Logger.Warn("Invalid token", zap.String("token", fmt.Sprintf("%+v", token)))
			return
		}

		//if user in admin group
		if keycloakToken.Groups[0] == os.Getenv("ADMIN_GROUP") && strings.ToLower(os.Getenv("TOKEN_EXCHANGE")) == "true" {

			// Generated by curl-to-Go: https://mholt.github.io/curl-to-go
			params := url.Values{}
			params.Add("client_id", `grafana`)
			params.Add("client_secret", os.Getenv("CLIENT_SECRET"))
			params.Add("subject_token", token.Raw)
			params.Add("requested_issuer", `openshift`)
			params.Add("grant_type", `urn:ietf:params:oauth:grant-type:token-exchange`)
			params.Add("audience", `grafana`)
			body := strings.NewReader(params.Encode())

			tokenExchangeRequest, err := http.NewRequest("POST", "https://sso.apps.play.gepaplexx.com/realms/internal/protocol/openid-connect/token", body)
			utils.LogError("Error with tokenExchangeRequest", err)
			tokenExchangeRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			resp, err := http.DefaultClient.Do(tokenExchangeRequest)
			utils.LogError("Error with doing token exchange request", err)
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			utils.LogError("Error parsing token exchange body", err)
			utils.Logger.Debug("TokenExchange successful")

			var result model.TokenExchange
			err = json.Unmarshal(b, &result)
			utils.LogError("Error unmarshalling TokenExchange struct", err)
			//request to bypass origin server
			req.Host = originBypassServerURL.Host
			req.URL.Host = originBypassServerURL.Host
			req.URL.Scheme = originBypassServerURL.Scheme
			req.Header.Set("Authorization", "Bearer "+result.AccessToken)

		} else {
			labels := labels_provider.GetLabelsFromRoleBindings(keycloakToken.PreferredUsername)
			// save the response from the origin server
			URL := req.URL.String()
			quIn := strings.Index(URL, "?") + 1
			labelsEnforcer := ""
			for _, label := range labels {
				labelsEnforcer += fmt.Sprintf("%s=%s&", tenantLabel, label)
			}
			req.URL, err = url.Parse(URL[:quIn] + labelsEnforcer + URL[quIn:])
			utils.LogError("Error while creating the namespace url", err)

			//proxy request to origin server
			req.Host = originServerURL.Host
			req.URL.Host = originServerURL.Host
			req.URL.Scheme = originServerURL.Scheme
			req.Header.Set("Authorization", "Bearer "+utils.ServiceAccountToken)
		}

		//clear request URI
		utils.Logger.Debug("Client request", zap.String("request", fmt.Sprintf("%+v", req)))
		req.RequestURI = ""
		originServerResponse, err := http.DefaultClient.Do(req)
		if err != nil {

			rw.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(rw, err)
			utils.LogError("Client request error", err)
			return
		}

		originBody, err := io.ReadAll(originServerResponse.Body)
		utils.LogError("Error reading origin server response body", err)
		utils.Logger.Debug("Upstream Response", zap.String("response", fmt.Sprintf("%+v", originServerResponse)), zap.String("body", fmt.Sprintf("%s", string(originBody))))

		// return response to the client
		rw.WriteHeader(http.StatusOK)
		_, err = rw.Write(originBody)
		utils.LogError("Error writing response to client", err)

		utils.Logger.Debug("Finished Client request")

		defer func(Body io.ReadCloser) {
			err := Body.Close()
			utils.LogError("Error closing body", err)
		}(originServerResponse.Body)
	}
}
