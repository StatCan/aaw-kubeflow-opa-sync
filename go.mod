module github.com/StatCan/kubeflow-opa-sync

go 1.15

require (
	github.com/StatCan/kubeflow-controller v0.0.0-20201013204235-7a61bdbec233 // indirect
	k8s.io/client-go v0.18.6
	k8s.io/klog v1.0.0 // indirect
)

replace github.com/googleapis/gnostic => github.com/googleapis/gnostic v0.4.0
