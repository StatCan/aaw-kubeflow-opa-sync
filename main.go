package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog"

	kubeflow "github.com/StatCan/kubeflow-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/kubeflow-controller/pkg/generated/informers/externalversions"
	kubeflowv1listers "github.com/StatCan/kubeflow-controller/pkg/generated/listers/kubeflowcontroller/v1"
	"k8s.io/client-go/informers"
	rbacv1listers "k8s.io/client-go/listers/rbac/v1"
)

const accessListExpiry = time.Minute

var kubeconfig string
var opaUrl string

var accessListExpires time.Time
var lastAccessList map[string][]string

// areEqual compares the two given access lists and checks if they are the same.
func areEqual(a map[string][]string, b map[string][]string) bool {
	// If we don't have the same number of keys
	if len(a) != len(b) {
		return false
	}

	// Check that all keys in "a" appear in "b"
	for ak, av := range a {
		if bv, ok := b[ak]; ok {
			// Ensure the same number of elements in the value of "a" and "b" at key
			if len(av) != len(bv) {
				return false
			}

			// Check that the values in "a" are identifical to the values in "b"
			//  note: we don't care about order in this case
			for _, va := range av {
				found := false

				for _, vb := range bv {
					// If the two values are the same, then we know the value appears in both "a" and "b"
					if va == vb {
						found = true
						break
					}
				}

				if !found {
					return false
				}
			}
		} else {
			return false
		}
	}

	return true
}

func generateAccessList(profilesLister kubeflowv1listers.ProfileLister, roleBindingsLister rbacv1listers.RoleBindingLister) error {
	access := make(map[string][]string)

	profiles, err := profilesLister.List(labels.Everything())
	if err != nil {
		return err
	}

	for _, profile := range profiles {
		access[profile.Name] = []string{profile.Spec.Owner.Name}

		// Find contributors in namespaces
		roleBindings, err := roleBindingsLister.RoleBindings(profile.Name).List(labels.Everything())
		if err != nil {
			return err
		}

		for _, roleBinding := range roleBindings {
			if val, ok := roleBinding.Annotations["role"]; !ok || val != "edit" {
				continue
			}

			for _, subject := range roleBinding.Subjects {
				if subject.APIGroup != "rbac.authorization.k8s.io" || subject.Kind != "User" {
					klog.Warningf("skipping non-user membership on role binding %s in namespace %s", roleBinding.Name, roleBinding.Namespace)
					continue
				}

				access[profile.Name] = append(access[profile.Name], subject.Name)
			}
		}
	}

	// Compare against the last access list we generated...
	// If it's different, then send it to the Open Policy Agent
	// 	or if it hasn't been updated in the last minute.
	if areEqual(lastAccessList, access) && time.Now().Before(accessListExpires) {
		klog.Infof("generated same access list and expiry hasn't passed.. skipping send to OPA")
		return nil
	}

	b, err := json.Marshal(access)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPut, opaUrl, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Add("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("unexpected status code submitting document: %d", res.StatusCode)
	}

	klog.Infof("updated document submitted to OPA")

	// Store the generated list for the next time
	lastAccessList = access
	accessListExpires = time.Now().Add(accessListExpiry)
	return nil
}

func main() {
	var err error
	ctx, cancel := context.WithCancel(context.Background())

	// Check if we are running inside the cluster, and default to using that instead of kubeconfig if that's the case
	_, err = os.Stat("/var/run/secrets/kubernetes.io/serviceaccount")

	// Setup the default path to the of the kubeconfig file
	if home := homedir.HomeDir(); os.IsNotExist(err) && home != "" {
		flag.StringVar(&kubeconfig, "kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	}

	flag.StringVar(&opaUrl, "opa-url", "http://localhost:8181/v1/data/statcan.gc.ca/daaas/profiles", "URL in OPA to write generated access list to")

	// Parse flags
	flag.Parse()

	// Construct the configuration based on the provided flags.
	// If no config file is provided, then the in-cluster config is used.
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		klog.Fatal(err)
	}

	// Start the Kubernetes client
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatal(err)
	}

	kubeflowclient, err := kubeflow.NewForConfig(config)
	if err != nil {
		klog.Fatal(err)
	}

	factory := informers.NewSharedInformerFactory(client, time.Minute)
	kubeflowFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowclient, time.Minute)

	// RoleBindings
	roleBindingsInformer := factory.Rbac().V1().RoleBindings()
	roleBindingsLister := roleBindingsInformer.Lister()

	// Profiles
	profilesInformer := kubeflowFactory.Kubeflow().V1().Profiles()
	profilesLister := profilesInformer.Lister()

	handlers := cache.ResourceEventHandlerFuncs{
		AddFunc: func(_ interface{}) {
			if !roleBindingsInformer.Informer().HasSynced() || !profilesInformer.Informer().HasSynced() {
				klog.Warningf("skipping add due to incomplete caches")
				return
			}

			err = generateAccessList(profilesLister, roleBindingsLister)
			if err != nil {
				klog.Errorf("failed generating access list: %v", err)
			}
		},
		UpdateFunc: func(_ interface{}, _ interface{}) {
			if !roleBindingsInformer.Informer().HasSynced() || !profilesInformer.Informer().HasSynced() {
				klog.Warningf("skipping update due to incomplete caches")
				return
			}

			err = generateAccessList(profilesLister, roleBindingsLister)
			if err != nil {
				klog.Errorf("failed generating access list: %v", err)
			}
		},
		DeleteFunc: func(_ interface{}) {
			if !roleBindingsInformer.Informer().HasSynced() || !profilesInformer.Informer().HasSynced() {
				klog.Warningf("skipping delete due to incomplete caches")
				return
			}

			err = generateAccessList(profilesLister, roleBindingsLister)
			if err != nil {
				klog.Errorf("failed generating access list: %v", err)
			}
		},
	}

	roleBindingsInformer.Informer().AddEventHandler(handlers)
	profilesInformer.Informer().AddEventHandler(handlers)

	go factory.Start(ctx.Done())
	go kubeflowFactory.Start(ctx.Done())

	// Wait until sync
	klog.Infof("synching caches...")
	tctx, _ := context.WithTimeout(ctx, time.Minute)
	if !cache.WaitForCacheSync(tctx.Done(), roleBindingsInformer.Informer().HasSynced, profilesInformer.Informer().HasSynced) {
		klog.Errorf("timeout synching caches")
		return
	}
	klog.Infof("done synching caches")

	klog.Infof("generating initial access list")
	err = generateAccessList(profilesLister, roleBindingsLister)
	if err != nil {
		klog.Fatalf("failed to generate initial access control list: %v", err)
	}

	c := make(chan os.Signal, 1)

	// We'll accept graceful shutdowns when quit via SIGINT (Ctrl+C)
	// SIGKILL, SIGQUIT or SIGTERM (Ctrl+/) will not be caught
	signal.Notify(c, os.Interrupt)

	// Block until we receive our signal
	<-c

	// Cancel global context
	cancel()

	log.Println("shutting down")
	os.Exit(0)
}
