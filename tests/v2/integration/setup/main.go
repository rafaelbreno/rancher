//go:build integrationsetup

// We're protecting this file with a build tag because it depends on github.com/containers/image which depends on C
// libraries that we can't and don't want to build unless we're going to run this integration setup program.

package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/creasty/defaults"
	rancherClient "github.com/rancher/shepherd/clients/rancher"
	management "github.com/rancher/shepherd/clients/rancher/generated/management/v3"
	"github.com/rancher/shepherd/extensions/token"
	"github.com/rancher/shepherd/pkg/config"
	"github.com/sirupsen/logrus"
	kwait "k8s.io/apimachinery/pkg/util/wait"
)

const (
	clusterNameBaseName = "integration-test-cluster"
)

// main creates a test namespace and cluster for use in integration tests.
func main() {
	// Make sure a valid cluster agent image tag was provided before doing anything else. The envvar CATTLE_AGENT_IMAGE
	// should be the image name (and tag) assigned to the cattle cluster agent image that was just built during CI.
	agentImage := os.Getenv("CATTLE_AGENT_IMAGE")
	if agentImage == "" {
		logrus.Fatal("Envvar CATTLE_AGENT_IMAGE must be set to a valid rancher-agent Docker image")
	}

	logrus.Infof("Generating test config")

	hostURL := fmt.Sprintf("%s:443", "localhost")

	var userToken *management.Token

	err := kwait.Poll(500*time.Millisecond, 5*time.Minute, func() (done bool, err error) {
		userToken, err = token.GenerateUserToken(&management.User{
			Username: "admin",
			Password: "admin",
		}, hostURL)
		if err != nil {
			logrus.Errorf("Pool error: %w", err)
			return false, nil
		}

		return true, nil
	})

	if err != nil {
		logrus.Fatalf("Error with generating admin token: %v", err)
	}

	cleanup := true
	rancherConfig := rancherClient.Config{
		AdminToken:  userToken.Token,
		Host:        hostURL,
		Cleanup:     &cleanup,
		ClusterName: "local",
	}

	err = defaults.Set(&rancherConfig)
	if err != nil {
		logrus.Fatalf("Error with setting up config file: %v", err)
	}

	err = config.WriteConfig(rancherClient.ConfigurationFileKey, &rancherConfig)
	if err != nil {
		logrus.Fatalf("Error writing test config: %v", err)
	}

	// Note that we do not defer clusterClients.Close() here. This is because doing so would cause the test namespace
	// in which the downstream cluster resides to be deleted before it can be used in tests.
}

// Get preferred outbound ip of this machine
func getOutboundIP() (net.IP, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	return conn.LocalAddr().(*net.UDPAddr).IP, nil
}
