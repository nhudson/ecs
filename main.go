package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/external"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/mitchellh/colorstring"
	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	client *ecs.ECS

	app    = kingpin.New("ecs", "ECS Tools")
	region = app.Flag("region", "AWS Region").Short('r').String()

	monitor        = app.Command("monitor", "List unhealthy services in your ECS clusters")
	monitorCluster = monitor.Flag("cluster", "Select the ECS cluster to monitor").String()
	filter         = monitor.Flag("filter", "Filter by the name of the ECS cluster").Short('f').String()
	longOutput     = monitor.Flag("long", "Enable detailed output of containers parameters").Short('l').Bool()
	printAll       = monitor.Flag("all", "Enable detailed output of containers parameters").Short('a').Bool()

	scaleService = app.Command("scale", "Scale the service to a specific DesiredCount")
	cluster      = scaleService.Flag("cluster", "Name of the ECS cluster").Required().String()
	service      = scaleService.Flag("service", "Name of the service").Required().String()
	desiredCount = scaleService.Flag("count", "New DesiredCount").Required().Int64()

	image        = app.Command("image", "Return the Docker image of a service running in ECS")
	imageCluster = image.Flag("cluster", "Name of the ECS cluster").Required().String()
	imageService = image.Flag("service", "Name of the service").Required().String()
)

func main() {
	res, err := app.Parse(os.Args[1:])
	cfg := loadAWSConfig()
	client = ecs.New(cfg)
	switch kingpin.MustParse(res, err) {
	case monitor.FullCommand():
		executeMonitor()
	case scaleService.FullCommand():
		executeScaleService()
	case image.FullCommand():
		executeServiceImage()
	}
}

func executeServiceImage() {
	ecsService, err := findService(*imageCluster, *imageService)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}
	taskDefinition := serviceTaskDefinition(&ecsService)
	for _, container := range taskDefinition.ContainerDefinitions {
		fmt.Println(*container.Image)
	}
}

func executeScaleService() {
	ecsService, err := findService(*cluster, *service)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}
	if *ecsService.DesiredCount == *desiredCount {
		fmt.Printf("Service %s already has a DesiredCount of %d",
			colorstring.Color("[yellow]"+*service), *desiredCount,
		)
		return
	}
	fmt.Printf(
		colorstring.Color("[yellow]Updating %s / DesiredCount[%d -> %d] RunningCount={%d}\n"),
		*service, *ecsService.DesiredCount, *desiredCount, *ecsService.RunningCount,
	)
	_, err = client.UpdateServiceRequest(&ecs.UpdateServiceInput{
		Cluster:      cluster,
		Service:      service,
		DesiredCount: desiredCount,
	}).Send()
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}
	fmt.Printf("Service %s successfully updated with DesiredCount=%d", *service, *desiredCount)
}

func findService(cluster, service string) (ecs.Service, error) {
	var ecsService ecs.Service
	runningServices := describeServices(cluster, []string{service})
	if len(runningServices) == 0 {
		fmt.Printf("No running service %s in cluster %s\n", service, cluster)
		return ecsService, errors.New("No service in cluster")
	}
	if len(runningServices) > 1 {
		fmt.Printf("Found more than 1 service named %s in cluster %s\n", service, cluster)
		return ecsService, errors.New("No service in cluster")
	}
	return runningServices[0], nil
}

func executeMonitor() {
	clusterNames := []string{*monitorCluster}
	if *monitorCluster == "" {
		clusterNames = listClusters(*filter)
	}
	for _, cluster := range describeClusters(clusterNames) {
		services := listServices(*cluster.ClusterName)
		headerLine := fmt.Sprintf(
			"--- CLUSTER: %s (%d services)\n",
			*cluster.ClusterName, len(services),
		)

		if *printAll == false {
			var displayedServices []ecs.Service
			for _, svc := range services {
				if !serviceOk(&svc) {
					displayedServices = append(displayedServices, svc)
				}
			}
			if len(displayedServices) > 0 {
				headerLine = fmt.Sprintf(
					"--- CLUSTER: %s (listing %d/%d services)\n",
					*cluster.ClusterName, len(displayedServices), len(services),
				)
				services = displayedServices
			}
		}
		fmt.Printf(headerLine)
		for _, svc := range services {
			printServiceDetails(&svc, *longOutput)
		}
		fmt.Println()
	}
}

func shortTaskDefinitionName(taskDefinition string) string {
	splitTaskDefinition := strings.Split(taskDefinition, "/")
	return strings.Split(taskDefinition, "/")[len(splitTaskDefinition)-1]
}

func listClusters(filter string) []string {
	clusterNames := []string{}
	listClusterOutput, err := client.ListClustersRequest(&ecs.ListClustersInput{}).Send()
	if err != nil {
		fmt.Printf("Failed to list clusters: " + err.Error())
		os.Exit(1)
	}

	if filter == "" {
		clusterNames = listClusterOutput.ClusterArns
	} else {
		for _, cluster := range listClusterOutput.ClusterArns {
			if strings.Contains(strings.ToLower(cluster), strings.ToLower(filter)) {
				clusterNames = append(clusterNames, cluster)
			}
		}
	}
	sort.Strings(clusterNames)
	return clusterNames
}

func describeClusters(clusters []string) []ecs.Cluster {
	descClusterReq := client.DescribeClustersRequest(&ecs.DescribeClustersInput{Clusters: clusters})
	descClusterOutput, err := descClusterReq.Send()
	if err != nil {
		fmt.Printf("Failed to describe clusters: " + err.Error())
		os.Exit(1)
	}
	sort.Slice(descClusterOutput.Clusters, func(i, j int) bool {
		return *descClusterOutput.Clusters[i].ClusterName < *descClusterOutput.Clusters[j].ClusterName
	})
	return descClusterOutput.Clusters
}

// List and describe services running in the ECS cluster
func listServices(cluster string) []ecs.Service {
	ecsServices := []ecs.Service{}
	serviceNames := []string{}
	req := client.ListServicesRequest(&ecs.ListServicesInput{Cluster: &cluster})
	pager := req.Paginate()
	for pager.Next() {
		page := pager.CurrentPage()
		serviceNames = append(serviceNames, page.ServiceArns...)
	}
	sort.Strings(serviceNames)
	for _, services := range chunk(serviceNames, 10) {
		if len(services) > 0 {
			descServices := describeServices(cluster, services)
			ecsServices = append(ecsServices, descServices...)
		}
	}
	return ecsServices
}

// Describe a list of services running in the ECS cluster
func describeServices(cluster string, services []string) []ecs.Service {
	params := ecs.DescribeServicesInput{Cluster: &cluster, Services: services}
	resp, err := client.DescribeServicesRequest(&params).Send()
	if err != nil {
		fmt.Printf("Failed to describe services: " + err.Error())
		os.Exit(1)
	}
	return resp.Services
}

// Split a list of strings into a list of smaller lists containing up to `count` items
func chunk(list []string, count int) [][]string {
	newList := make([][]string, len(list)/count+1)
	for index := 0; index < len(list); index += count {
		upperBound := index + count
		if index+count > len(list) {
			upperBound = len(list)
		}
		newIdx := index / count
		newList[newIdx] = list[index:upperBound]
	}
	return newList
}

// Load AWS configuration
func loadAWSConfig() aws.Config {
	cfg, err := external.LoadDefaultAWSConfig()
	if err != nil {
		panic("Failed to load AWS SDK configuration, " + err.Error())
	}
	if *region != "" {
		cfg.Region = *region
	}
	return cfg
}

func serviceUp(service *ecs.Service) bool {
	return *service.DesiredCount == *service.RunningCount &&
		len(service.Events) > 0 &&
		strings.Contains(*service.Events[0].Message, "has reached a steady state")
}

func serviceStatus(service *ecs.Service) string {
	status := colorstring.Color("[green][OK]")
	serviceUp := serviceUp(service)
	if !serviceUp {
		status = colorstring.Color("[red][KO]")
	}
	if serviceUp && *service.RunningCount == 0 {
		status = colorstring.Color("[yellow][WARN]")
	}
	return status
}

func serviceOk(service *ecs.Service) bool {
	status := serviceStatus(service)
	return strings.Contains(status, "OK")
}

func serviceTaskDefinition(service *ecs.Service) ecs.TaskDefinition {
	resp, err := client.DescribeTaskDefinitionRequest(
		&ecs.DescribeTaskDefinitionInput{TaskDefinition: service.TaskDefinition}).Send()
	if err != nil {
		fmt.Printf("Failed to describe task definition: " + err.Error())
		os.Exit(1)
	}
	return *resp.TaskDefinition
}

func printServiceDetails(service *ecs.Service, longOutput bool) {
	fmt.Printf(
		"%-15s %-60s %-8s running %d/%d  (%s)\n",
		serviceStatus(service),
		colorstring.Color("[yellow]"+*service.ServiceName),
		*service.Status, *service.RunningCount, *service.DesiredCount,
		shortTaskDefinitionName(*service.TaskDefinition),
	)
	if longOutput == true {
		taskDefinition := serviceTaskDefinition(service)
		for _, container := range taskDefinition.ContainerDefinitions {
			portsString := []string{}
			for _, ports := range container.PortMappings {
				portsString = append(portsString, "->"+strconv.FormatInt(*ports.ContainerPort, 10))
			}
			fmt.Printf("- Container: %s\n", *container.Name)
			fmt.Printf("  Image: %s\n", *container.Image)
			fmt.Printf("  Memory: %d / CPU: %d\n", *container.Memory, *container.Cpu)
			fmt.Printf("  Ports: %s\n", strings.Join(portsString, " "))
			fmt.Println("  Environment:")
			for _, env := range container.Environment {
				fmt.Printf("   - %s: %s\n", *env.Name, *env.Value)
			}
		}
		fmt.Println()
	}
}