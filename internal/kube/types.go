package kube

const Prefix = "postgres-operator.crunchydata.com/"
const ClusterAPI = "postgres-operator.crunchydata.com/v1beta1"

type Owner struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

type Metadata struct {
	Namespace         string            `json:"namespace"`
	Name              string            `json:"name"`
	UID               string            `json:"uid"`
	CreationTimestamp string            `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
	Owners            []Owner           `json:"ownerReferences"`
}

type Backup struct {
	Repo           string `json:"repo"`
	Succeeded      int    `json:"succeeded"`
	CompletionTime string `json:"completionTime"`
}

type Cluster struct {
	Metadata Metadata `json:"metadata"`
	Spec     struct {
		Backups struct {
			PGBackRest struct {
				Repos []struct {
					Name string `json:"name"`
				} `json:"repos"`
			} `json:"pgbackrest"`
		} `json:"backups"`
	} `json:"spec"`
	Status struct {
		PGBackRest struct {
			ScheduledBackups []Backup `json:"scheduledBackups"`
		} `json:"pgbackrest"`
	} `json:"status"`
}

type Job struct {
	Metadata Metadata `json:"metadata"`
	Status   struct {
		Succeeded      int    `json:"succeeded"`
		CompletionTime string `json:"completionTime"`
		Conditions     []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

type CronJob struct {
	Metadata Metadata `json:"metadata"`
	Status   struct {
		LastSuccessfulTime string `json:"lastSuccessfulTime"`
	} `json:"status"`
}

type Sample struct {
	Clusters []Cluster
	Jobs     []Job
	CronJobs []CronJob
}
