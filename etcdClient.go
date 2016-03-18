package objdb

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/net/context"

	log "github.com/Sirupsen/logrus"
	"github.com/coreos/etcd/client"
)

type etcdPlugin struct {
	client client.Client // etcd client
	kapi   client.KeysAPI

	serviceDb map[string]*serviceState
	mutex     *sync.Mutex
}

type member struct {
	Name       string   `json:"name"`
	ClientURLs []string `json:"clientURLs"`
}

type memData struct {
	Members []member `json:"members"`
}

// Register the plugin
func init() {
	RegisterPlugin("etcd", &etcdPlugin{mutex: new(sync.Mutex)})
}

// Initialize the etcd client
func (ep *etcdPlugin) Init(endpoints []string) error {
	var err error

	ep.mutex.Lock()
	defer ep.mutex.Unlock()

	// Setup default url
	if len(endpoints) == 0 {
		endpoints = []string{"http://127.0.0.1:2379"}
	}

	etcdConfig := client.Config{
		Endpoints: endpoints,
	}

	// Create a new client
	ep.client, err = client.New(etcdConfig)
	if err != nil {
		log.Fatal("Error creating etcd client. Err: %v", err)
		return err
	}

	// create keys api
	ep.kapi = client.NewKeysAPI(ep.client)

	// Initialize service DB
	ep.serviceDb = make(map[string]*serviceState)

	return nil
}

// Get an object
func (ep *etcdPlugin) GetObj(key string, retVal interface{}) error {
	keyName := "/contiv.io/obj/" + key

	// Get the object from etcd client
	resp, err := ep.kapi.Get(context.Background(), keyName, nil)
	if err != nil {
		log.Errorf("Error getting key %s. Err: %v", keyName, err)
		return err
	}

	// Parse JSON response
	if err := json.Unmarshal([]byte(resp.Node.Value), retVal); err != nil {
		log.Errorf("Error parsing object %s, Err %v", resp.Node.Value, err)
		return err
	}

	return nil
}

// Recursive function to look thru each directory and get the files
func recursAddNode(node *client.Node, list []string) []string {
	for _, innerNode := range node.Nodes {
		// add only the files.
		if !innerNode.Dir {
			list = append(list, innerNode.Value)
		} else {
			list = recursAddNode(innerNode, list)
		}
	}

	return list
}

// Get a list of objects in a directory
func (ep *etcdPlugin) ListDir(key string) ([]string, error) {
	keyName := "/contiv.io/obj/" + key

	// Get the object from etcd client
	resp, err := ep.kapi.Get(context.Background(), keyName, &client.GetOptions{Recursive: true, Sort: true})
	if err != nil {
		return nil, nil
	}

	if !resp.Node.Dir {
		log.Errorf("ListDir response is not a directory")
		return nil, errors.New("Response is not directory")
	}

	var retList []string
	// Call a recursive function to recurse thru each directory and get all files
	// Warning: assumes directory itep is not interesting to the caller
	// Warning2: there is also an assumption that keynames are not required
	//           Which means, caller has to derive the key from value :(
	retList = recursAddNode(resp.Node, retList)

	return retList, nil
}

// Save an object, create if it doesnt exist
func (ep *etcdPlugin) SetObj(key string, value interface{}) error {
	keyName := "/contiv.io/obj/" + key

	// JSON format the object
	jsonVal, err := json.Marshal(value)
	if err != nil {
		log.Errorf("Json conversion error. Err %v", err)
		return err
	}

	// Set it via etcd client
	if _, err := ep.kapi.Set(context.Background(), keyName, string(jsonVal[:]), nil); err != nil {
		log.Errorf("Error setting key %s, Err: %v", keyName, err)
		return err
	}

	return nil
}

// Remove an object
func (ep *etcdPlugin) DelObj(key string) error {
	keyName := "/contiv.io/obj/" + key

	// Remove it via etcd client
	if _, err := ep.kapi.Delete(context.Background(), keyName, nil); err != nil {
		log.Errorf("Error removing key %s, Err: %v", keyName, err)
		return err
	}

	return nil
}

// Get JSON output from a http request
func httpGetJSON(url string, data interface{}) (interface{}, error) {
	res, err := http.Get(url)
	if err != nil {
		log.Errorf("Error during http get. Err: %v", err)
		return nil, err
	}
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		log.Errorf("Error during ioutil readall. Err: %v", err)
		return nil, err
	}

	if err := json.Unmarshal(body, data); err != nil {
		log.Errorf("Error during json unmarshall. Err: %v", err)
		return nil, err
	}

	log.Debugf("Results for (%s): %+v\n", url, data)

	return data, nil
}

// Return the local address where etcd is listening
func (ep *etcdPlugin) GetLocalAddr() (string, error) {
	var epData struct {
		Name string `json:"name"`
	}

	// Get ep state from etcd
	if _, err := httpGetJSON("http://localhost:2379/v2/stats/self", &epData); err != nil {
		log.Errorf("Error getting self state. Err: %v", err)
		return "", errors.New("Error getting self state")
	}

	var memData memData

	// Get member list from etcd
	if _, err := httpGetJSON("http://localhost:2379/v2/members", &memData); err != nil {
		log.Errorf("Error getting members state. Err: %v", err)
		return "", errors.New("Error getting members state")
	}

	myName := epData.Name
	members := memData.Members

	for _, mem := range members {
		if mem.Name == myName {
			for _, clientURL := range mem.ClientURLs {
				hostStr := strings.TrimPrefix(clientURL, "http://")
				hostAddr := strings.Split(hostStr, ":")[0]
				log.Infof("Got host addr: %s", hostAddr)
				return hostAddr, nil
			}
		}
	}
	return "", errors.New("Address not found")
}
