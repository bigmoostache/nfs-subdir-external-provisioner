/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/golang/glog"
	v1 "k8s.io/api/core/v1"

	storage "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	storagehelpers "k8s.io/component-helpers/storage/volume"
	"sigs.k8s.io/sig-storage-lib-external-provisioner/v6/controller"
    "k8s.io/apimachinery/pkg/api/resource"
)

const (
	provisionerNameKey = "PROVISIONER_NAME"
)

type nfsProvisioner struct {
	client kubernetes.Interface
	server string
	path   string
}

type pvcMetadata struct {
	data        map[string]string
	labels      map[string]string
	annotations map[string]string
}

var pattern = regexp.MustCompile(`\${\.PVC\.((labels|annotations)\.(.*?)|.*?)}`)

func (meta *pvcMetadata) stringParser(str string) string {
	result := pattern.FindAllStringSubmatch(str, -1)
	for _, r := range result {
		switch r[2] {
		case "labels":
			str = strings.ReplaceAll(str, r[0], meta.labels[r[3]])
		case "annotations":
			str = strings.ReplaceAll(str, r[0], meta.annotations[r[3]])
		default:
			str = strings.ReplaceAll(str, r[0], meta.data[r[1]])
		}
	}

	return str
}

const (
	mountPath = "/persistentvolumes"
	disksFilePath = "/persistentvolumes/disks.txt"
)

var _ controller.Provisioner = &nfsProvisioner{}

// add_allocated_size adds the allocated size to disks.txt
func (p *nfsProvisioner) add_allocated_size(pvcname string, n_octets int64) bool {
    fmt.Println("The value of pvcname is:", pvcname)
    fmt.Println("The value of n_octets is:", n_octets)
	totalToAllocateStr := os.Getenv("TOTAL_TO_ALLOCATE")
    fmt.Println("The value of totalToAllocateStr is:", totalToAllocateStr)
	if totalToAllocateStr == "" {
		return false
	}
	totalToAllocate, err := strconv.ParseInt(totalToAllocateStr, 10, 64)
	if err != nil {
		return false
	}
    fmt.Println("Total to allocate is:", totalToAllocate)

	file, err := os.OpenFile(disksFilePath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		glog.Errorf("failed to open disks.txt: %v", err)
		return false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var allocatedSize int64
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			continue
		}
		name := parts[0]
		size, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		if name == pvcname {
			glog.Errorf("pvcname %s already exists in disks.txt", pvcname)
			return false
		}
		allocatedSize += size
	}

	if allocatedSize+n_octets > totalToAllocate {
		return false
	}

	if _, err := file.WriteString(fmt.Sprintf("%s,%d\n", pvcname, n_octets)); err != nil {
		glog.Errorf("failed to write to disks.txt: %v", err)
		return false
	}

	return true
}

// delete_allocated_size deletes the allocated size entry from disks.txt
func (p *nfsProvisioner) delete_allocated_size(pvcname string) bool {
	fmt.Println("Deleting from disks.txt: %v", pvcname)
	file, err := os.OpenFile(disksFilePath, os.O_RDWR, 0o644)
	if err != nil {
		glog.Errorf("failed to open disks.txt: %v", err)
		return false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var lines []string
	found := false
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			continue
		}
		name := parts[0]
		if name == pvcname {
			fmt.Println("Pruning: %v", name)
			found = true
			continue
		}
		fmt.Println("Keeping: %v", name)
		lines = append(lines, line)
	}

	if !found {
		glog.Warningf("pvcname %s not found in disks.txt", pvcname)
		return false
	}

	if err := file.Truncate(0); err != nil {
		glog.Errorf("failed to truncate disks.txt: %v", err)
		return false
	}

	if _, err := file.Seek(0, 0); err != nil {
		glog.Errorf("failed to seek in disks.txt: %v", err)
		return false
	}

	for _, line := range lines {
		if _, err := file.WriteString(line + "\n"); err != nil {
			glog.Errorf("failed to write to disks.txt: %v", err)
			return false
		}
	}

	return true
}

func (p *nfsProvisioner) Provision(ctx context.Context, options controller.ProvisionOptions) (*v1.PersistentVolume, controller.ProvisioningState, error) {
	if options.PVC.Spec.Selector != nil {
		return nil, controller.ProvisioningFinished, fmt.Errorf("claim Selector is not supported")
	}
	fmt.Printf("nfs provisioner: VolumeOptions %v", options)

	pvcNamespace := options.PVC.Namespace
	pvcName := options.PVC.Name

	pvName := strings.Join([]string{pvcNamespace, pvcName, options.PVName}, "-")

	metadata := &pvcMetadata{
		data: map[string]string{
			"name":      pvcName,
			"namespace": pvcNamespace,
		},
		labels:      options.PVC.Labels,
		annotations: options.PVC.Annotations,
	}

	fullPath := filepath.Join(mountPath, pvName)
	path := filepath.Join(p.path, pvName)

	pathPattern, exists := options.StorageClass.Parameters["pathPattern"]
	if exists {
		customPath := metadata.stringParser(pathPattern)
		if customPath != "" {
			path = filepath.Join(p.path, customPath)
			fullPath = filepath.Join(mountPath, customPath)
		}
	}

	glog.V(4).Infof("creating path %s", fullPath)
	if err := os.MkdirAll(fullPath, 0o777); err != nil {
		return nil, controller.ProvisioningFinished, errors.New("unable to create directory to provision new pv: " + err.Error())
	}
	err := os.Chmod(fullPath, 0o777)
	if err != nil {
		return nil, "", err
	}
	
    fmt.Printf("Computing stuff: %d\n", pvName)
	allocatedSizeQuantity, exists := options.PVC.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)]
	if !exists {
		return nil, controller.ProvisioningFinished, errors.New("storage size not specified in PVC")
	}
    fmt.Printf("Computing stuff: %d\n", allocatedSizeQuantity)
	allocatedSize := allocatedSizeQuantity.ScaledValue(resource.Mega)
    fmt.Printf("Total number of Mi: %d\n", allocatedSize)	
	if !p.add_allocated_size(options.PVName, allocatedSize) {
		return nil, controller.ProvisioningFinished, errors.New("unable to allocate the requested size")
	}

	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: options.PVName,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: *options.StorageClass.ReclaimPolicy,
			AccessModes:                   options.PVC.Spec.AccessModes,
			MountOptions:                  options.StorageClass.MountOptions,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): options.PVC.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)],
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				NFS: &v1.NFSVolumeSource{
					Server:   p.server,
					Path:     path,
					ReadOnly: false,
				},
			},
		},
	}
	return pv, controller.ProvisioningFinished, nil
}

func (p *nfsProvisioner) Delete(ctx context.Context, volume *v1.PersistentVolume) error {
	path := volume.Spec.PersistentVolumeSource.NFS.Path
	fmt.Printf("Deleting: %d\n", path)
	basePath := filepath.Base(path)
	fmt.Printf("Deleting: %d\n", basePath)
	oldPath := strings.Replace(path, p.path, mountPath, 1)
	fmt.Printf("Archiving to: %d\n", oldPath)

	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		glog.Warningf("path %s does not exist, deletion skipped", oldPath)
		return nil
	}
	// Get the storage class for this volume.
	storageClass, err := p.getClassForVolume(ctx, volume)
	if err != nil {
		return err
	}

	// Determine if the "onDelete" parameter exists.
	// If it exists and has a `delete` value, delete the directory.
	// If it exists and has a `retain` value, safe the directory.
	onDelete := storageClass.Parameters["onDelete"]
	fmt.Printf("Just cleaning up... %s\n", onDelete)
	switch onDelete {
	case "delete":
		fmt.Printf("Deleting completely")
		p.delete_allocated_size(volume.Name)
		return os.RemoveAll(oldPath)
	case "retain":
		fmt.Printf("Retaining For some reason")
		p.delete_allocated_size(volume.Name)
		return nil
	default:
		fmt.Printf("Just cleaning up...")
		p.delete_allocated_size(volume.Name)
		return nil
	}

	// Determine if the "archiveOnDelete" parameter exists.
	// If it exists and has a false value, delete the directory.
	// Otherwise, archive it.
	archiveOnDelete, exists := storageClass.Parameters["archiveOnDelete"]
	if exists {
		archiveBool, err := strconv.ParseBool(archiveOnDelete)
		if err != nil {
			return err
		}
		if !archiveBool {
			return os.RemoveAll(oldPath)
		}
	}

	archivePath := filepath.Join(mountPath, "archived-"+basePath)
	glog.V(4).Infof("archiving path %s to %s", oldPath, archivePath)
	return os.Rename(oldPath, archivePath)
}

// getClassForVolume returns StorageClass.
func (p *nfsProvisioner) getClassForVolume(ctx context.Context, pv *v1.PersistentVolume) (*storage.StorageClass, error) {
	if p.client == nil {
		return nil, fmt.Errorf("cannot get kube client")
	}
	className := storagehelpers.GetPersistentVolumeClass(pv)
	if className == "" {
		return nil, fmt.Errorf("volume has no storage class")
	}
	class, err := p.client.StorageV1().StorageClasses().Get(ctx, className, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return class, nil
}

func main() {
	flag.Parse()
	flag.Set("logtostderr", "true")

	server := os.Getenv("NFS_SERVER")
	if server == "" {
		glog.Fatal("NFS_SERVER not set")
	}
	path := os.Getenv("NFS_PATH")
	if path == "" {
		glog.Fatal("NFS_PATH not set")
	}
	provisionerName := os.Getenv(provisionerNameKey)
	if provisionerName == "" {
		glog.Fatalf("environment variable %s is not set! Please set it.", provisionerNameKey)
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	var config *rest.Config
	if kubeconfig != "" {
		// Create an OutOfClusterConfig and use it to create a client for the controller
		// to use to communicate with Kubernetes
		var err error
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			glog.Fatalf("Failed to create kubeconfig: %v", err)
		}
	} else {
		// Create an InClusterConfig and use it to create a client for the controller
		// to use to communicate with Kubernetes
		var err error
		config, err = rest.InClusterConfig()
		if err != nil {
			glog.Fatalf("Failed to create config: %v", err)
		}
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Failed to create client: %v", err)
	}

	// The controller needs to know what the server version is because out-of-tree
	// provisioners aren't officially supported until 1.5
	serverVersion, err := clientset.Discovery().ServerVersion()
	if err != nil {
		glog.Fatalf("Error getting server version: %v", err)
	}

	leaderElection := true
	leaderElectionEnv := os.Getenv("ENABLE_LEADER_ELECTION")
	if leaderElectionEnv != "" {
		leaderElection, err = strconv.ParseBool(leaderElectionEnv)
		if err != nil {
			glog.Fatalf("Unable to parse ENABLE_LEADER_ELECTION env var: %v", err)
		}
	}

	clientNFSProvisioner := &nfsProvisioner{
		client: clientset,
		server: server,
		path:   path,
	}

	// Check if disks.txt exists, and create it if it does not.
	if _, err := os.Stat(disksFilePath); os.IsNotExist(err) {
		fmt.Println("Creating disks.txt")
		file, err := os.Create(disksFilePath)
		if err != nil {
			fmt.Println("I failed...")
			glog.Fatalf("Failed to create disks.txt: %v", err)
		}
		file.Close()
		fmt.Println("Created disks.txt")
	}
	// Start the provision controller which will dynamically provision efs NFS
	// PVs
	pc := controller.NewProvisionController(clientset,
		provisionerName,
		clientNFSProvisioner,
		serverVersion.GitVersion,
		controller.LeaderElection(leaderElection),
	)
	// Never stops.
	pc.Run(context.Background())
}
