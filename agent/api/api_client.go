// Copyright 2014-2015 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package api

import (
	"crypto/tls"
	"errors"
	"runtime"

	"github.com/aws/amazon-ecs-agent/agent/ecs_client/awsjson/codec"
	"github.com/aws/amazon-ecs-agent/agent/ecs_client/client/dialer"
	svc "github.com/aws/amazon-ecs-agent/agent/ecs_client/ecs_autogenerated_client"

	"github.com/docker/docker/pkg/system"

	"github.com/aws/amazon-ecs-agent/agent/ecs_client/authv4"
	"github.com/aws/amazon-ecs-agent/agent/ecs_client/authv4/credentials"

	"github.com/aws/amazon-ecs-agent/agent/config"
	"github.com/aws/amazon-ecs-agent/agent/ec2"
	"github.com/aws/amazon-ecs-agent/agent/logger"
	"github.com/aws/amazon-ecs-agent/agent/utils"
)

var log = logger.ForModule("api client")

type ECSClient interface {
	CredentialProvider() credentials.AWSCredentialProvider
	RegisterContainerInstance() (string, error)
	SubmitTaskStateChange(change ContainerStateChange) utils.RetriableError
	SubmitContainerStateChange(change ContainerStateChange) utils.RetriableError
	DiscoverPollEndpoint(containerInstanceArn string) (string, error)
	DeregisterContainerInstance(containerInstanceArn string) error
}

type ApiECSClient struct {
	credentialProvider credentials.AWSCredentialProvider
	config             *config.Config
	insecureSkipVerify bool
}

const (
	ECS_SERVICE = "ecs"
)

// serviceClient recreates a new service clent and signer with each request.
// This is because there is some question of whether the connection pool used by
// the client is valid.
func (client *ApiECSClient) serviceClient() (*svc.AmazonEC2ContainerServiceV20141113Client, error) {
	config := client.config

	signer := authv4.NewHttpSigner(config.AWSRegion, ECS_SERVICE, client.CredentialProvider(), nil)

	c := codec.AwsJson{Host: config.APIEndpoint, SignerV4: signer}

	d, err := dialer.TLS(config.APIEndpoint, config.APIPort, &tls.Config{InsecureSkipVerify: client.insecureSkipVerify})

	if err != nil {
		log.Error("Cannot resolve url", "url", config.APIEndpoint, "port", config.APIPort, "err", err)
		return nil, err
	}

	ecs := svc.NewAmazonEC2ContainerServiceV20141113Client(d, c)
	return ecs, nil
}

func NewECSClient(credentialProvider credentials.AWSCredentialProvider, config *config.Config, insecureSkipVerify bool) ECSClient {
	return &ApiECSClient{credentialProvider: credentialProvider, config: config, insecureSkipVerify: insecureSkipVerify}
}

func (client *ApiECSClient) CredentialProvider() credentials.AWSCredentialProvider {
	return client.credentialProvider
}

func getCpuAndMemory() (int32, int32) {
	memInfo, err := system.ReadMemInfo()
	mem := int32(memInfo.MemTotal / 1024 / 1024) // MB
	if err != nil {
		log.Error("Unable to get memory info", "err", err)
		mem = 0
	}
	cpu := runtime.NumCPU() * 1024

	return int32(cpu), mem
}

// CreateCluster creates a cluster from a given name and returns its arn
func (client *ApiECSClient) CreateCluster(clusterName string) (string, error) {
	svcRequest := svc.NewCreateClusterRequest()
	svcRequest.SetClusterName(&clusterName)

	svcClient, err := client.serviceClient()
	if err != nil {
		log.Error("Unable to get service client for frontend", "err", err)
		return "", err
	}

	resp, err := svcClient.CreateCluster(svcRequest)
	if err != nil {
		log.Crit("Could not create cluster", "err", err)
		return "", err
	}
	log.Info("Created a cluster!", "clusterName", clusterName)
	return *resp.Cluster().ClusterArn(), nil

}

func (client *ApiECSClient) describeCluster(clusterName string) (clusterArn string, clusterStatus string, err error) {
	svcRequest := svc.NewDescribeClustersRequest()
	clusterNames := []*string{&clusterName}
	svcRequest.SetClusters(clusterNames)

	svcClient, err := client.serviceClient()
	if err != nil {
		log.Error("Unable to get service client for frontend", "err", err)
		return
	}

	resp, err := svcClient.DescribeClusters(svcRequest)
	if err != nil {
		log.Error("Unable to describe cluster", "cluster", clusterName, "err", err)
		return
	}
	for _, cluster := range resp.Clusters() {
		if *cluster.ClusterName() == clusterName {
			clusterArn = *cluster.ClusterArn()
			clusterStatus = *cluster.Status()
			return
		}
	}
	return
}

func (client *ApiECSClient) RegisterContainerInstance() (string, error) {
	clusterArn := client.config.ClusterArn
	// If our clusterArn is empty, we should try to create the default
	if clusterArn == "" {
		clusterArn = config.DEFAULT_CLUSTER_NAME
		defer func() {
			// Update the config value to reflect the cluster we end up in
			client.config.ClusterArn = clusterArn
		}()
		// Attempt to register without checking existence of the cluster so we don't require
		// excess permissions in the case where the cluster already exists and is active
		containerInstanceArn, err := client.registerContainerInstance(clusterArn)
		if err == nil {
			return containerInstanceArn, nil
		}
		// If trying to register fails, see if the cluster exists and is active
		clusterArn, clusterStatus, err := client.describeCluster(clusterArn)
		if err != nil {
			return "", err
		}
		// Assume that an inactive cluster is intentional and do not recreate it
		if clusterStatus != "" && clusterStatus != "ACTIVE" {
			message := "Cluster is not available for registration"
			log.Error(message, "cluster", clusterArn)
			return "", errors.New(message)
		}
		clusterArn, err = client.CreateCluster(clusterArn)
		if err != nil {
			return "", err
		}
	}
	return client.registerContainerInstance(clusterArn)
}

func (client *ApiECSClient) registerContainerInstance(clusterArn string) (string, error) {
	svcRequest := svc.NewRegisterContainerInstanceRequest()
	svcRequest.SetCluster(&clusterArn)

	ec2MetadataClient := ec2.NewEC2MetadataClient()
	instanceIdentityDoc, err := ec2MetadataClient.ReadResource(ec2.INSTANCE_IDENTITY_DOCUMENT_RESOURCE)
	iidRetrieved := true
	if err != nil {
		log.Error("Unable to get instance identity document", "err", err)
		iidRetrieved = false
		instanceIdentityDoc = []byte{}
	}
	strIid := string(instanceIdentityDoc)
	svcRequest.SetInstanceIdentityDocument(&strIid)

	instanceIdentitySignature := []byte{}
	if iidRetrieved {
		instanceIdentitySignature, err = ec2MetadataClient.ReadResource(ec2.INSTANCE_IDENTITY_DOCUMENT_SIGNATURE_RESOURCE)
		if err != nil {
			log.Error("Unable to get instance identity signature", "err", err)
		}
	}

	strIidSig := string(instanceIdentitySignature)
	svcRequest.SetInstanceIdentityDocumentSignature(&strIidSig)

	integerStr := "INTEGER"

	cpu, mem := getCpuAndMemory()

	cpuResource := svc.NewResource()
	cpuResource.SetName(utils.Strptr("CPU"))
	cpuResource.SetType(&integerStr)
	cpuResource.SetIntegerValue(&cpu)

	memResource := svc.NewResource()
	memResource.SetName(utils.Strptr("MEMORY"))
	memResource.SetType(&integerStr)
	memResource.SetIntegerValue(&mem)

	portResource := svc.NewResource()
	portResource.SetName(utils.Strptr("PORTS"))
	portResource.SetType(utils.Strptr("STRINGSET"))
	portResource.SetStringSetValue(utils.Uint16SliceToStringSlice(client.config.ReservedPorts))

	resources := []svc.Resource{cpuResource, memResource, portResource}
	svcRequest.SetTotalResources(resources)

	ecs, err := client.serviceClient()
	if err != nil {
		log.Error("Unable to get service client for frontend", "err", err)
		return "", err
	}

	resp, err := ecs.RegisterContainerInstance(svcRequest)
	if err != nil {
		log.Error("Could not register", "err", err)
		return "", err
	}
	log.Info("Registered!")
	return *resp.ContainerInstance().ContainerInstanceArn(), nil
}

func (client *ApiECSClient) SubmitTaskStateChange(change ContainerStateChange) utils.RetriableError {
	if change.TaskStatus == TaskStatusNone {
		log.Warn("SubmitTaskStateChange called with an invalid change", "change", change)
		return NewStateChangeError(errors.New("SubmitTaskStateChange called with an invalid change"))
	}

	stat := change.TaskStatus.String()
	if stat == "DEAD" {
		stat = "STOPPED"
	}
	if stat != "STOPPED" && stat != "RUNNING" {
		log.Debug("Not submitting unsupported upstream task state", "state", stat)
		// Not really an error
		return nil
	}

	req := svc.NewSubmitTaskStateChangeRequest()
	req.SetTask(&change.TaskArn)
	req.SetStatus(&stat)
	req.SetCluster(&client.config.ClusterArn)

	c, err := client.serviceClient()
	if err != nil {
		return NewStateChangeError(err)
	}
	_, err = c.SubmitTaskStateChange(req)
	if err != nil {
		log.Warn("Could not submit a task state change", "err", err)
		return NewStateChangeError(err)
	}
	return nil
}

func (client *ApiECSClient) SubmitContainerStateChange(change ContainerStateChange) utils.RetriableError {
	req := svc.NewSubmitContainerStateChangeRequest()
	req.SetTask(&change.TaskArn)
	req.SetContainerName(&change.ContainerName)
	stat := change.Status.String()
	if stat == "DEAD" {
		stat = "STOPPED"
	}
	if stat != "STOPPED" && stat != "RUNNING" {
		log.Info("Not submitting not supported upstream container state", "state", stat)
		return nil
	}
	req.SetStatus(&stat)
	req.SetCluster(&client.config.ClusterArn)
	if change.ExitCode != nil {
		exitCode := int32(*change.ExitCode)
		req.SetExitCode(&exitCode)
	}
	networkBindings := make([]svc.NetworkBinding, len(change.PortBindings))
	for i, binding := range change.PortBindings {
		aBinding := svc.NewNetworkBinding()
		aBinding.SetBindIP(&binding.BindIp)
		hostPort := int32(binding.HostPort)
		aBinding.SetHostPort(&hostPort)
		containerPort := int32(binding.ContainerPort)
		aBinding.SetContainerPort(&containerPort)
		networkBindings[i] = aBinding
	}
	req.SetNetworkBindings(networkBindings)

	c, err := client.serviceClient()
	if err != nil {
		return NewStateChangeError(err)
	}
	_, err = c.SubmitContainerStateChange(req)
	if err != nil {
		log.Warn("Could not submit a container state change", "change", change, "err", err)
		return NewStateChangeError(err)
	}
	return nil
}

func (client *ApiECSClient) DiscoverPollEndpoint(containerInstanceArn string) (string, error) {
	req := svc.NewDiscoverPollEndpointRequest()
	req.SetContainerInstance(&containerInstanceArn)
	req.SetCluster(&client.config.ClusterArn)

	c, err := client.serviceClient()
	if err != nil {
		return "", err
	}

	resp, err := c.DiscoverPollEndpoint(req)
	if err != nil {
		return "", err
	}

	return *resp.Endpoint(), nil
}

func (client *ApiECSClient) DeregisterContainerInstance(containerInstanceArn string) error {
	req := svc.NewDeregisterContainerInstanceRequest()
	req.SetCluster(&client.config.ClusterArn)
	req.SetContainerInstance(&containerInstanceArn)

	c, err := client.serviceClient()
	if err != nil {
		return err
	}

	_, err = c.DeregisterContainerInstance(req)

	return err
}
