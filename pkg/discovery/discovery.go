package discovery

import (
	"context"
	"crypto/tls"
	cert "crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/coreos/etcd/client"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	serviceHost    string
	servicePort    string
	Namespace      string
	httpMethod     string
	etcdServiceURL string

	KindPluralMap  map[string]string
	kindVersionMap map[string]string
	compositionMap map[string][]string

	REPLICA_SET  string
	DEPLOYMENT   string
	POD          string
	CONFIG_MAP   string
	SERVICE      string
	SECRET       string
	PVCLAIM      string
	PV           string
	ETCD_CLUSTER string
)

var (
	masterURL   string
	kubeconfig  string
	etcdservers string
)

func init() {

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&etcdservers, "etcd-servers", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")

	flag.Parse()
	serviceHost = os.Getenv("KUBERNETES_SERVICE_HOST")
	servicePort = os.Getenv("KUBERNETES_SERVICE_PORT")
	Namespace = "default"
	httpMethod = http.MethodGet

	etcdServiceURL = "http://localhost:2379"

	DEPLOYMENT = "Deployment"
	REPLICA_SET = "ReplicaSet"
	POD = "Pod"
	CONFIG_MAP = "ConfigMap"
	SERVICE = "Service"
	SECRET = "Secret"
	PVCLAIM = "PersistentVolumeClaim"
	PV = "PersistentVolume"
	ETCD_CLUSTER = "EtcdCluster"

	KindPluralMap = make(map[string]string)
	kindVersionMap = make(map[string]string)
	compositionMap = make(map[string][]string, 0)

	readKindCompositionFile()

	// set basic data types
	KindPluralMap[DEPLOYMENT] = "deployments"
	kindVersionMap[DEPLOYMENT] = "apis/apps/v1"
	compositionMap[DEPLOYMENT] = []string{"ReplicaSet"}

	KindPluralMap[REPLICA_SET] = "replicasets"
	kindVersionMap[REPLICA_SET] = "apis/extensions/v1beta1"
	compositionMap[REPLICA_SET] = []string{"Pod"}

	KindPluralMap[POD] = "pods"
	kindVersionMap[POD] = "api/v1"
	compositionMap[POD] = []string{}

	KindPluralMap[SERVICE] = "services"
	kindVersionMap[SERVICE] = "api/v1"
	compositionMap[SERVICE] = []string{}

	KindPluralMap[SECRET] = "secrets"
	kindVersionMap[SECRET] = "api/v1"
	compositionMap[SECRET] = []string{}

	KindPluralMap[PVCLAIM] = "persistentvolumeclaims"
	kindVersionMap[PVCLAIM] = "api/v1"
	compositionMap[PVCLAIM] = []string{}

	KindPluralMap[PV] = "persistentvolumes"
	kindVersionMap[PV] = "api/v1/persistentvolumes"
	compositionMap[PV] = []string{}
}

func BuildCompositionTree() {
	for {
		readKindCompositionFile()
		resourceKindList := getResourceKinds()
		resourceInCluster := []MetaDataAndOwnerReferences{}
		for _, resourceKind := range resourceKindList {
			topLevelMetaDataOwnerRefList := getResourceNames(resourceKind)
			//fmt.Printf("TopLevelMetaDataOwnerRefList:%v\n", topLevelMetaDataOwnerRefList)
			for _, topLevelObject := range topLevelMetaDataOwnerRefList {
				resourceName := topLevelObject.MetaDataName

				level := 1
				compositionTree := []CompositionTreeNode{}
				buildProvenance(resourceKind, resourceName, level, &compositionTree)
				//fmt.Printf("CompositionTree:%v\n", compositionTree)
				TotalClusterProvenance.storeProvenance(topLevelObject, resourceKind, resourceName, &compositionTree)
			}
			for _, resource := range topLevelMetaDataOwnerRefList {
				present := false
				for _, res := range resourceInCluster {
					if res.MetaDataName == resource.MetaDataName {
						present = true
					}
				}
				if !present {
					resourceInCluster = append(resourceInCluster, resource)
				}
			}
		}

		TotalClusterProvenance.purgeCompositionOfDeletedItems(resourceInCluster)

		time.Sleep(time.Second * 10)
	}
}

func (cp *ClusterProvenance) checkIfProvenanceNeeded(resourceKind, resourceName string) bool {
	cp.mux.Lock()
	defer cp.mux.Unlock()
	for _, provenanceItem := range cp.clusterProvenance {
		kind := provenanceItem.Kind
		name := provenanceItem.Name
		if resourceKind == kind && resourceName == name {
			return false
		}
	}
	return true
}

func readKindCompositionFile() {
	// read from the opt file
	filePath, ok := os.LookupEnv("KIND_COMPOSITION_FILE")
	if ok {
		yamlFile, err := ioutil.ReadFile(filePath)
		if err != nil {
			fmt.Printf("Error reading file:%s", err)
		}

		compositionsList := make([]composition, 0)
		err = yaml.Unmarshal(yamlFile, &compositionsList)

		for _, compositionObj := range compositionsList {
			kind := compositionObj.Kind
			endpoint := compositionObj.Endpoint
			composition := compositionObj.Composition
			plural := compositionObj.Plural
			//fmt.Printf("Kind:%s, Plural: %s Endpoint:%s, Composition:%s\n", kind, plural, endpoint, composition)

			KindPluralMap[kind] = plural
			kindVersionMap[kind] = endpoint
			compositionMap[kind] = composition
		}
	} else {
		// Populate the Kind maps by querying CRDs from ETCD and querying KAPI for details of each CRD
		crdListString := queryETCD("/operators")
		if crdListString != "" {
			crdNameList := getCRDNames(crdListString)

			for _, crdName := range crdNameList {
				crdDetailsString := queryETCD("/" + crdName)
				kind, plural, endpoint, composition := getCRDDetails(crdDetailsString)

				KindPluralMap[kind] = plural
				kindVersionMap[kind] = endpoint
				compositionMap[kind] = composition
			}
		}
	}
	//printMaps()
}

func getCRDNames(crdListString string) []string {
	var operatorMapList []map[string]map[string]interface{}
	var operatorDataMap map[string]interface{}

	if err := json.Unmarshal([]byte(crdListString), &operatorMapList); err != nil {
		fmt.Printf("Error:%s\n", err.Error())
	}

	var crdNameList []string = make([]string, 0)
	for _, operator := range operatorMapList {
		operatorDataMap = operator["Operator"]

		customResources := operatorDataMap["CustomResources"]

		for _, cr := range customResources.([]interface{}) {
			crdNameList = append(crdNameList, cr.(string))
		}
	}
	return crdNameList
}

func getCRDDetails(crdDetailsString string) (string, string, string, []string) {

	var crdDetailsMap = make(map[string]interface{})
	kind := ""
	plural := ""
	endpoint := ""
	composition := make([]string, 0)

	if err := json.Unmarshal([]byte(crdDetailsString), &crdDetailsMap); err != nil {
		fmt.Printf("Error:%s\n", err.Error())
	}

	kind = crdDetailsMap["kind"].(string)
	endpoint = crdDetailsMap["endpoint"].(string)
	plural = crdDetailsMap["plural"].(string)
	compositionString := crdDetailsMap["composition"].(string)
	composition1 := strings.Split(compositionString, ",")
	for _, elem := range composition1 {
		elem = strings.TrimSpace(elem)
		composition = append(composition, elem)
	}

	return kind, plural, endpoint, composition
}

func GetOpenAPISpec(customResourceKind string) string {

	// 1. Get ConfigMap Name by querying etcd at
	resourceKey := "/" + customResourceKind + "-OpenAPISpecConfigMap"
	configMapNameString := queryETCD(resourceKey)

	var configMapName string
	if err := json.Unmarshal([]byte(configMapNameString), &configMapName); err != nil {
		fmt.Printf("Error:%s\n", err.Error())
	}

	// 2. Query ConfigMap
	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		fmt.Printf("Error:%s\n", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Printf("Error:%s\n", err.Error())
	}

	configMap, err := kubeClient.CoreV1().ConfigMaps("default").Get(configMapName, metav1.GetOptions{})

	if err != nil {
		fmt.Printf("Error:%s\n", err.Error())
	}

	configMapData := configMap.Data
	openAPISpec := configMapData["openapispec"]

	return openAPISpec
}

func queryETCD(resourceKey string) string {
	cfg := client.Config{
		Endpoints: []string{etcdServiceURL},
		Transport: client.DefaultTransport,
	}
	c, err := client.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	kapi := client.NewKeysAPI(c)

	resp, err1 := kapi.Get(context.Background(), resourceKey, nil)
	if err1 != nil {
		return string(err1.Error())
	} else {
		return resp.Node.Value
	}
	return ""
}

func printMaps() {
	fmt.Println("Printing kindVersionMap")
	for key, value := range kindVersionMap {
		fmt.Printf("%s, %s\n", key, value)
	}
	fmt.Println("Printing KindPluralMap")
	for key, value := range KindPluralMap {
		fmt.Printf("%s, %s\n", key, value)
	}
	fmt.Println("Printing compositionMap")
	for key, value := range compositionMap {
		fmt.Printf("%s, %s\n", key, value)
	}
}

func getResourceKinds() []string {
	resourceKindSlice := make([]string, 0)
	for key, _ := range compositionMap {
		resourceKindSlice = append(resourceKindSlice, key)
	}
	return resourceKindSlice
}

func getResourceNames(resourceKind string) []MetaDataAndOwnerReferences {
	resourceApiVersion := kindVersionMap[resourceKind]
	resourceKindPlural := KindPluralMap[resourceKind]
	content := getResourceListContent(resourceApiVersion, resourceKindPlural)
	metaDataAndOwnerReferenceList := parseMetaData(content)
	return metaDataAndOwnerReferenceList
}

func (cp *ClusterProvenance) PrintProvenance() {
	cp.mux.Lock()
	defer cp.mux.Unlock()
	fmt.Println("Provenance of different Kinds in this Cluster")
	for _, provenanceItem := range cp.clusterProvenance {
		kind := provenanceItem.Kind
		name := provenanceItem.Name
		compositionTree := provenanceItem.CompositionTree
		fmt.Printf("Kind: %s Name: %s Composition:\n", kind, name)
		for _, compositionTreeNode := range *compositionTree {
			level := compositionTreeNode.Level
			childKind := compositionTreeNode.ChildKind
			metaDataAndOwnerReferences := compositionTreeNode.Children
			for _, metaDataNode := range metaDataAndOwnerReferences {
				childName := metaDataNode.MetaDataName
				childStatus := metaDataNode.Status
				fmt.Printf("  %d %s %s %s\n", level, childKind, childName, childStatus)
			}
		}
		fmt.Println("============================================")
	}
}

func processed(processedList *[]CompositionTreeNode, nodeToCheck CompositionTreeNode) bool {
	//fmt.Printf("ProcessedList:%v\n", processedList)
	//fmt.Printf("NodeToCheck:%v\n", nodeToCheck)
	var result bool = false
	for _, compositionTreeNode1 := range *processedList {
		if compositionTreeNode1.Level == nodeToCheck.Level && compositionTreeNode1.ChildKind == nodeToCheck.ChildKind {
			result = true
		}
	}
	return result
}

func getComposition(kind, name, status string, level int, compositionTree *[]CompositionTreeNode,
	processedList *[]CompositionTreeNode) Composition {
	//var provenanceString string
	//fmt.Printf("-- Kind: %s Name: %s\n", kind, name)
	//provenanceString = "Kind: " + kind + " Name:" + name + " Composition:\n"
	parentComposition := Composition{}
	parentComposition.Level = level
	parentComposition.Kind = kind
	parentComposition.Name = name
	parentComposition.Status = status
	parentComposition.Children = []Composition{}

	//fmt.Printf("CompositionTree:%v\n", compositionTree)

	for _, compositionTreeNode := range *compositionTree {
		if processed(processedList, compositionTreeNode) {
			continue
		}
		level := compositionTreeNode.Level
		childKind := compositionTreeNode.ChildKind
		metaDataAndOwnerReferences := compositionTreeNode.Children

		for _, metaDataNode := range metaDataAndOwnerReferences {
			//provenanceString = provenanceString + " " + string(level) + " " + childKind + " " + childName + "\n"
			childName := metaDataNode.MetaDataName
			childStatus := metaDataNode.Status
			trimmedTree := []CompositionTreeNode{}
			for _, compositionTreeNode1 := range *compositionTree {
				if compositionTreeNode1.Level != level && compositionTreeNode1.ChildKind != childKind {
					trimmedTree = append(trimmedTree, compositionTreeNode1)
				}
			}
			*processedList = append(*processedList, compositionTreeNode)
			child := getComposition(childKind, childName, childStatus, level, &trimmedTree, processedList)
			parentComposition.Children = append(parentComposition.Children, child)
			compositionTree = &[]CompositionTreeNode{}
		}
	}
	return parentComposition
}

func getComposition1(kind, name, status string, compositionTree *[]CompositionTreeNode) Composition {
	var provenanceString string
	fmt.Printf("Kind: %s Name: %s Composition:\n", kind, name)
	provenanceString = "Kind: " + kind + " Name:" + name + " Composition:\n"
	parentComposition := Composition{}
	parentComposition.Level = 0
	parentComposition.Kind = kind
	parentComposition.Name = name
	parentComposition.Status = status
	parentComposition.Children = []Composition{}
	var root = parentComposition
	for _, compositionTreeNode := range *compositionTree {
		level := compositionTreeNode.Level
		childKind := compositionTreeNode.ChildKind
		metaDataAndOwnerReferences := compositionTreeNode.Children
		//childComposition.Children = []Composition{}
		var childrenList = []Composition{}
		for _, metaDataNode := range metaDataAndOwnerReferences {
			childComposition := Composition{}
			childName := metaDataNode.MetaDataName
			childStatus := metaDataNode.Status
			fmt.Printf("  %d %s %s\n", level, childKind, childName)
			provenanceString = provenanceString + " " + string(level) + " " + childKind + " " + childName + "\n"
			childComposition.Level = level
			childComposition.Kind = childKind
			childComposition.Name = childName
			childComposition.Status = childStatus
			childrenList = append(childrenList, childComposition)
		}
		root.Children = childrenList
		fmt.Printf("Root composition:%v\n", root)
		root = root.Children[0]
	}
	return parentComposition
}

func (cp *ClusterProvenance) GetProvenance(resourceKind, resourceName string) string {
	cp.mux.Lock()
	defer cp.mux.Unlock()
	var provenanceBytes []byte
	var provenanceString string
	compositions := []Composition{}

	resourceKindPlural := KindPluralMap[resourceKind]

	//fmt.Println("Provenance of different Kinds in this Cluster")
	//fmt.Printf("Kind:%s, Name:%s\n", resourceKindPlural, resourceName)
	for _, provenanceItem := range cp.clusterProvenance {
		kind := strings.ToLower(provenanceItem.Kind)
		name := strings.ToLower(provenanceItem.Name)
		status := provenanceItem.Status
		compositionTree := provenanceItem.CompositionTree
		resourceKindPlural := strings.ToLower(resourceKindPlural)
		//TODO(devdattakulkarni): Make route registration and provenance keyed info
		//to use same kind name (plural). Currently Provenance info is keyed on
		//singular kind names. For now, trimming the 's' at the end
		//resourceKind = strings.TrimSuffix(resourceKind, "s")
		var resourceKind string
		for key, value := range KindPluralMap {
			if strings.ToLower(value) == strings.ToLower(resourceKindPlural) {
				resourceKind = strings.ToLower(key)
				break
			}
		}
		resourceName := strings.ToLower(resourceName)
		//fmt.Printf("Kind:%s, Kind:%s, Name:%s, Name:%s\n", kind, resourceKind, name, resourceName)
		if resourceName == "*" {
			if resourceKind == kind {
				processedList := []CompositionTreeNode{}
				level := 1
				composition := getComposition(kind, name, status, level, compositionTree, &processedList)
				compositions = append(compositions, composition)
			}
		} else if resourceKind == kind && resourceName == name {
			processedList := []CompositionTreeNode{}
			level := 1
			composition := getComposition(kind, name, status, level, compositionTree, &processedList)
			compositions = append(compositions, composition)
		}
	}

	provenanceBytes, err := json.Marshal(compositions)
	if err != nil {
		fmt.Println(err)
	}
	provenanceString = string(provenanceBytes)
	return provenanceString
}

func (cp *ClusterProvenance) purgeCompositionOfDeletedItems(topLevelMetaDataOwnerRefList []MetaDataAndOwnerReferences) {
	presentList := []Provenance{}
	//fmt.Println("ClusterProvenance:%v\n", cp.clusterProvenance)
	//fmt.Println("ToplevelMetaDataOwnerList:%v\n", topLevelMetaDataOwnerRefList)
	for _, prov := range cp.clusterProvenance {
		for _, topLevelObject := range topLevelMetaDataOwnerRefList {
			resourceName := topLevelObject.MetaDataName
			//fmt.Printf("ResourceName:%s, prov.Name:%s\n", resourceName, prov.Name)
			if resourceName == prov.Name {
				presentList = append(presentList, prov)
			}
		}
	}
	//fmt.Printf("Updated Cluster Prov List:%v\n", presentList)
	cp.clusterProvenance = presentList
}

// This stores Provenance information in memory. The provenance information will be lost
// when this Pod is deleted.
func (cp *ClusterProvenance) storeProvenance(topLevelObject MetaDataAndOwnerReferences,
	resourceKind string, resourceName string,
	compositionTree *[]CompositionTreeNode) {
	cp.mux.Lock()
	defer cp.mux.Unlock()
	provenance := Provenance{
		Kind:            resourceKind,
		Name:            resourceName,
		Status:          topLevelObject.Status,
		CompositionTree: compositionTree,
	}
	present := false
	// If prov already exists then replace status and composition Tree
	//fmt.Printf("00 CP:%v\n", cp.clusterProvenance)
	for i, prov := range cp.clusterProvenance {
		if prov.Kind == provenance.Kind && prov.Name == provenance.Name {
			present = true
			p := &prov
			//fmt.Printf("CompositionTree:%v\n", compositionTree)
			p.CompositionTree = compositionTree
			p.Status = topLevelObject.Status
			cp.clusterProvenance[i] = *p
			//fmt.Printf("11 CP:%v\n", cp.clusterProvenance)
		}
	}
	if !present {
		cp.clusterProvenance = append(cp.clusterProvenance, provenance)
		//fmt.Printf("22 CP:%v\n", cp.clusterProvenance)
	}
	//fmt.Println("Exiting storeprovenance")
	//fmt.Printf("ClusterProvenance:%v\n", cp.clusterProvenance)
}

// This stores Provenance information in etcd accessible at the etcdServiceURL
// One option to deploy etcd is to use the CoreOS etcd-operator.
// The etcdServiceURL initialized in init() is for the example etcd cluster that
// will be created by the etcd-operator. See https://github.com/coreos/etcd-operator
//Ref:https://github.com/coreos/etcd/tree/master/client
func storeProvenance_etcd(resourceKind string, resourceName string, compositionTree *[]CompositionTreeNode) {
	//fmt.Println("Entering storeProvenance")
	jsonCompositionTree, err := json.Marshal(compositionTree)
	if err != nil {
		panic(err)
	}
	resourceProv := string(jsonCompositionTree)
	cfg := client.Config{
		//Endpoints: []string{"http://192.168.99.100:32379"},
		Endpoints: []string{etcdServiceURL},
		Transport: client.DefaultTransport,
		// set timeout per request to fail fast when the target endpoint is unavailable
		//HeaderTimeoutPerRequest: time.Second,
	}
	//fmt.Printf("%v\n", cfg)
	c, err := client.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	kapi := client.NewKeysAPI(c)
	// set "/foo" key with "bar" value
	//resourceKey := "/compositions/Deployment/pod42test-deployment"
	//resourceProv := "{1 ReplicaSet; 2 Pod -1}"
	resourceKey := string("/compositions/" + resourceKind + "/" + resourceName)
	fmt.Printf("Setting %s->%s\n", resourceKey, resourceProv)
	resp, err := kapi.Set(context.Background(), resourceKey, resourceProv, nil)
	if err != nil {
		log.Fatal(err)
	} else {
		// print common key info
		log.Printf("Set is done. Metadata is %q\n", resp)
	}
	//fmt.Printf("Getting value for %s\n", resourceKey)
	resp, err = kapi.Get(context.Background(), resourceKey, nil)
	if err != nil {
		log.Fatal(err)
	} else {
		// print common key info
		//log.Printf("Get is done. Metadata is %q\n", resp)
		// print value
		log.Printf("%q key has %q value\n", resp.Node.Key, resp.Node.Value)
	}
	//fmt.Println("Exiting storeProvenance")
}

func buildProvenance(parentResourceKind string, parentResourceName string, level int,
	compositionTree *[]CompositionTreeNode) {
	childResourceKindList, present := compositionMap[parentResourceKind]
	if present {
		level = level + 1

		for _, childResourceKind := range childResourceKindList {
			childKindPlural := KindPluralMap[childResourceKind]
			childResourceApiVersion := kindVersionMap[childResourceKind]
			var content []byte
			var metaDataAndOwnerReferenceList []MetaDataAndOwnerReferences
			content = getResourceListContent(childResourceApiVersion, childKindPlural)
			metaDataAndOwnerReferenceList = parseMetaData(content)

			childrenList := filterChildren(&metaDataAndOwnerReferenceList, parentResourceName)
			compTreeNode := CompositionTreeNode{
				Level:     level,
				ChildKind: childResourceKind,
				Children:  childrenList,
			}

			*compositionTree = append(*compositionTree, compTreeNode)

			for _, metaDataRef := range childrenList {
				resourceName := metaDataRef.MetaDataName
				resourceKind := childResourceKind
				buildProvenance(resourceKind, resourceName, level, compositionTree)
			}
		}
	} else {
		return
	}
}

func getResourceListContent(resourceApiVersion, resourcePlural string) []byte {
	//fmt.Println("Entering getResourceListContent")
	var url1 string
	if !strings.Contains(resourceApiVersion, resourcePlural) {
	   url1 = fmt.Sprintf("https://%s:%s/%s/namespaces/%s/%s", serviceHost, servicePort, resourceApiVersion, Namespace, resourcePlural)
	} else {
	  url1 = fmt.Sprintf("https://%s:%s/%s", serviceHost, servicePort, resourceApiVersion)
	}
	//fmt.Printf("Url:%s\n",url1)
	caToken := getToken()
	caCertPool := getCACert()
	u, err := url.Parse(url1)
	if err != nil {
		panic(err)
	}
	req, err := http.NewRequest(httpMethod, u.String(), nil)
	if err != nil {
		fmt.Println(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", string(caToken)))
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: caCertPool,
			},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("sending request failed: %s", err.Error())
		fmt.Println(err)
	}
	defer resp.Body.Close()
	resp_body, _ := ioutil.ReadAll(resp.Body)

	//fmt.Println(resp.Status)
	//fmt.Println(string(resp_body))
	//fmt.Println("Exiting getResourceListContent")
	return resp_body
}

//Ref:https://www.sohamkamani.com/blog/2017/10/18/parsing-json-in-golang/#unstructured-data
func parseMetaData(content []byte) []MetaDataAndOwnerReferences {
	//fmt.Println("Entering parseMetaData")
	var result map[string]interface{}
	json.Unmarshal([]byte(content), &result)
	// We need to parse following from the result
	// metadata.name
	// metadata.ownerReferences.name
	// metadata.ownerReferences.kind
	// metadata.ownerReferences.apiVersion
	metaDataSlice := []MetaDataAndOwnerReferences{}
	items, ok := result["items"].([]interface{})

	if ok {
		for _, item := range items {
			//fmt.Println("=======================")
			itemConverted := item.(map[string]interface{})
			var metadataProcessed, statusProcessed bool
			metaDataRef := MetaDataAndOwnerReferences{}
			statusKeyExists := false
			for key, _ := range itemConverted {
			    if key == "status" {
			       statusKeyExists = true
			    }
			}
			for key, value := range itemConverted {
				if key == "metadata" {
					//fmt.Println("----")
					//fmt.Println(key, value.(interface{}))
					metadataMap := value.(map[string]interface{})
					for mkey, mvalue := range metadataMap {
						//fmt.Printf("%v ==> %v\n", mkey, mvalue.(interface{}))
						if mkey == "ownerReferences" {
							ownerReferencesList := mvalue.([]interface{})
							for _, ownerReference := range ownerReferencesList {
								ownerReferenceMap := ownerReference.(map[string]interface{})
								for okey, ovalue := range ownerReferenceMap {
									//fmt.Printf("%v --> %v\n", okey, ovalue)
									if okey == "name" {
										metaDataRef.OwnerReferenceName = ovalue.(string)
									}
									if okey == "kind" {
										metaDataRef.OwnerReferenceKind = ovalue.(string)
									}
									if okey == "apiVersion" {
										metaDataRef.OwnerReferenceAPIVersion = ovalue.(string)
									}
								}
							}
						}
						if mkey == "name" {
							metaDataRef.MetaDataName = mvalue.(string)
						}
					}
					metadataProcessed = true
				}
				if key == "status" {
					statusMap := value.(map[string]interface{})
					var replicas, readyReplicas, availableReplicas float64
					for skey, svalue := range statusMap {
						if skey == "phase" {
							metaDataRef.Status = svalue.(string)
							//fmt.Printf("Status:%s\n", metaDataRef.Status)
						}
						if skey == "replicas" {
							replicas = svalue.(float64)
						}
						if skey == "readyReplicas" {
							readyReplicas = svalue.(float64)
						}
						if skey == "availableReplicas" {
							availableReplicas = svalue.(float64)
						}
					}
					// Trying to be completely sure that we can set READY status
					if replicas > 0 {
						if replicas == availableReplicas && replicas == readyReplicas {
							metaDataRef.Status = "Ready"
						}
					}
					statusProcessed = true
				}
				if statusKeyExists {
				   if metadataProcessed && statusProcessed {
					metaDataSlice = append(metaDataSlice, metaDataRef)
				   }
				} else if metadataProcessed {
				  metaDataSlice = append(metaDataSlice, metaDataRef)
				}
			}
		}
	}
	//fmt.Println("Exiting parseMetaData")
	//fmt.Printf("Metadata slice:%v\n", metaDataSlice)
	return metaDataSlice
}

func filterChildren(metaDataSlice *[]MetaDataAndOwnerReferences, parentResourceName string) []MetaDataAndOwnerReferences {
	metaDataSliceToReturn := []MetaDataAndOwnerReferences{}
	for _, metaDataRef := range *metaDataSlice {
		if metaDataRef.OwnerReferenceName == parentResourceName {
			// Prevent duplicates
			present := false
			for _, node := range metaDataSliceToReturn {
				if node.MetaDataName == metaDataRef.MetaDataName {
					present = true
				}
			}
			if !present {
				metaDataSliceToReturn = append(metaDataSliceToReturn, metaDataRef)
			}
		}
	}
	return metaDataSliceToReturn
}

// Ref:https://stackoverflow.com/questions/30690186/how-do-i-access-the-kubernetes-api-from-within-a-pod-container
func getToken() []byte {
	caToken, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		panic(err) // cannot find token file
	}
	//fmt.Printf("Token:%s", caToken)
	return caToken
}

// Ref:https://stackoverflow.com/questions/30690186/how-do-i-access-the-kubernetes-api-from-within-a-pod-container
func getCACert() *cert.CertPool {
	caCertPool := cert.NewCertPool()
	caCert, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		panic(err) // Can't find cert file
	}
	//fmt.Printf("CaCert:%s",caCert)
	caCertPool.AppendCertsFromPEM(caCert)
	return caCertPool
}
