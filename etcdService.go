package objdb

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/context"

	log "github.com/Sirupsen/logrus"
	"github.com/coreos/etcd/client"
)

const serviceTTL = 30

// Service state
type serviceState struct {
	ServiceName string // Name of the service
	HostAddr    string // Host name or IP address where its running
	Port        int    // Port number where its listening

	// Channel to stop ttl refresh
	stopChan chan bool
}

// Register a service
// Service is registered with a ttl for 60sec and a goroutine is created
// to refresh the ttl.
func (ep *etcdPlugin) RegisterService(serviceInfo ServiceInfo) error {
	keyName := "/contiv.io/service/" + serviceInfo.ServiceName + "/" +
		serviceInfo.HostAddr + ":" + strconv.Itoa(serviceInfo.Port)

	log.Infof("Registering service key: %s, value: %+v", keyName, serviceInfo)

	// if there is a previously registered service, de-register it
	if ep.serviceDb[keyName] != nil {
		ep.DeregisterService(serviceInfo)
	}

	// JSON format the object
	jsonVal, err := json.Marshal(serviceInfo)
	if err != nil {
		log.Errorf("Json conversion error. Err %v", err)
		return err
	}

	// Set it via etcd client
	_, err = ep.kapi.Set(context.Background(), keyName, string(jsonVal[:]), &client.SetOptions{TTL: serviceTTL})
	if err != nil {
		log.Errorf("Error setting key %s, Err: %v", keyName, err)
		return err
	}

	// Run refresh in background
	stopChan := make(chan bool, 1)
	go refreshService(ep.kapi, keyName, string(jsonVal[:]), stopChan)

	// Store it in DB
	ep.serviceDb[keyName] = &serviceState{
		ServiceName: serviceInfo.ServiceName,
		HostAddr:    serviceInfo.HostAddr,
		Port:        serviceInfo.Port,
		stopChan:    stopChan,
	}

	return nil
}

// GetService lists all end points for a service
func (ep *etcdPlugin) GetService(name string) ([]ServiceInfo, error) {
	keyName := "/contiv.io/service/" + name + "/"

	_, srvcList, err := ep.getServiceState(keyName)
	return srvcList, err
}

func (ep *etcdPlugin) getServiceState(key string) (uint64, []ServiceInfo, error) {
	var srvcList []ServiceInfo

	// Get the object from etcd client
	resp, err := ep.kapi.Get(context.Background(), key, &client.GetOptions{Recursive: true, Sort: true})
	if err != nil {
		if strings.Contains(err.Error(), "Key not found") {
			return 0, nil, nil
		}

		log.Errorf("Error getting key %s. Err: %v", key, err)
		return 0, nil, err
	}

	if !resp.Node.Dir {
		log.Errorf("Err. Response is not a directory: %+v", resp.Node)
		return 0, nil, errors.New("Invalid Response from etcd")
	}

	// Parse each node in the directory
	for _, node := range resp.Node.Nodes {
		var respSrvc ServiceInfo
		// Parse JSON response
		err = json.Unmarshal([]byte(node.Value), &respSrvc)
		if err != nil {
			log.Errorf("Error parsing object %s, Err %v", node.Value, err)
			return 0, nil, err
		}

		srvcList = append(srvcList, respSrvc)
	}

	watchIndex := resp.Index + 1
	return watchIndex, srvcList, nil
}

// initServiceState reads the current state and injects it to the channel
// additionally, it returns the next index to watch
func (ep *etcdPlugin) initServiceState(key string, eventCh chan WatchServiceEvent) (uint64, error) {
	mIndex, srvcList, err := ep.getServiceState(key)
	if err != nil {
		return mIndex, err
	}

	// walk each service and inject it as an add event
	for _, srvInfo := range srvcList {
		log.Infof("Sending service add event: %+v", srvInfo)
		// Send Add event
		eventCh <- WatchServiceEvent{
			EventType:   WatchServiceEventAdd,
			ServiceInfo: srvInfo,
		}
	}

	return mIndex, nil
}

// Watch for a service
func (ep *etcdPlugin) WatchService(name string,
	eventCh chan WatchServiceEvent, stopCh chan bool) error {
	keyName := "/contiv.io/service/" + name + "/"

	// Create channels
	watchCh := make(chan *client.Response, 1)

	// Create watch context
	watchCtx, watchCancel := context.WithCancel(context.Background())

	// Start the watch thread
	go func() {
		// Get current state and etcd index to watch
		watchIndex, err := ep.initServiceState(keyName, eventCh)
		if err != nil {
			log.Fatalf("Unable to watch service key: %s - %v", keyName,
				err)
		}

		log.Infof("Watching for service: %s at index %v", keyName, watchIndex)
		// Start the watch
		watcher := ep.kapi.Watcher(keyName, &client.WatcherOptions{AfterIndex: watchIndex, Recursive: true})
		if watcher == nil {
			log.Errorf("Error watching service %s. Etcd returned invalid watcher", keyName)

			// Emit the event
			eventCh <- WatchServiceEvent{EventType: WatchServiceEventError}
		}

		// Keep getting next event
		for {
			// Block till next watch event
			etcdRsp, err := watcher.Next(watchCtx)
			if err != nil && err.Error() == client.ErrClusterUnavailable.Error() {
				log.Infof("Stopping watch on key %s", keyName)
				return
			} else if err != nil {
				log.Errorf("Error %v during watch. Watch thread exiting", err)
				return
			}

			// Send it to watch channel
			watchCh <- etcdRsp
		}
	}()

	// handle messages from watch service
	go func() {
		for {
			select {
			case watchResp := <-watchCh:
				log.Debugf("Received event %#v\n Node: %#v", watchResp, watchResp.Node)

				// derive service info from key
				srvKey := strings.TrimPrefix(watchResp.Node.Key, "/contiv.io/service/")
				parts := strings.Split(srvKey, "/")
				if len(parts) < 2 {
					log.Warnf("Recieved event for key %q, could not parse service key", srvKey)
					break
				}

				srvName := parts[0]
				hostAddr := parts[1]

				parts = strings.Split(hostAddr, ":")
				if len(parts) != 2 {
					log.Warnf("Recieved event for key %q, could not parse hostinfo", srvKey)
					break
				}

				hostAddr = parts[0]
				portNum, _ := strconv.Atoi(parts[1])

				// Build service info
				srvInfo := ServiceInfo{
					ServiceName: srvName,
					HostAddr:    hostAddr,
					Port:        portNum,
				}

				// We ignore all events except Set/Delete/Expire
				// Note that Set event doesnt exactly mean new service end point.
				// If a service restarts and re-registers before it expired, we'll
				// receive set again. receivers need to handle this case
				if watchResp.Action == "set" {
					log.Infof("Sending service add event: %+v", srvInfo)
					// Send Add event
					eventCh <- WatchServiceEvent{
						EventType:   WatchServiceEventAdd,
						ServiceInfo: srvInfo,
					}
				} else if (watchResp.Action == "delete") ||
					(watchResp.Action == "expire") {

					log.Infof("Sending service del event: %+v", srvInfo)

					// Send Delete event
					eventCh <- WatchServiceEvent{
						EventType:   WatchServiceEventDel,
						ServiceInfo: srvInfo,
					}
				}
			case stopReq := <-stopCh:
				if stopReq {
					// Stop watch and return
					log.Infof("Stopping watch on %s", keyName)
					watchCancel()
					return
				}
			}
		}
	}()

	return nil
}

// Deregister a service
// This removes the service from the registry and stops the refresh groutine
func (ep *etcdPlugin) DeregisterService(serviceInfo ServiceInfo) error {
	keyName := "/contiv.io/service/" + serviceInfo.ServiceName + "/" +
		serviceInfo.HostAddr + ":" + strconv.Itoa(serviceInfo.Port)

	// Find it in the database
	srvState := ep.serviceDb[keyName]
	if srvState == nil {
		log.Errorf("Could not find the service in db %s", keyName)
		return errors.New("Service not found")
	}

	// stop the refresh thread and delete service
	srvState.stopChan <- true
	delete(ep.serviceDb, keyName)

	// Delete the service instance
	_, err := ep.kapi.Delete(context.Background(), keyName, nil)
	if err != nil {
		log.Errorf("Error deleting key %s. Err: %v", keyName, err)
		return err
	}

	return nil
}

// Keep refreshing the service every 30sec
func refreshService(kapi client.KeysAPI, keyName string, keyVal string, stopChan chan bool) {
	for {
		select {
		case <-time.After(time.Second * time.Duration(serviceTTL/3)):
			log.Debugf("Refreshing key: %s", keyName)

			_, err := kapi.Set(context.Background(), keyName, keyVal, &client.SetOptions{TTL: serviceTTL})
			if err != nil {
				log.Warnf("Error updating key %s, Err: %v", keyName, err)
				// In case of a TTL expiry, this key may have been deleted
				// from the etcd db. Hence use of Set instead of Update
				_, err := kapi.Set(context.Background(), keyName, keyVal, &client.SetOptions{TTL: serviceTTL})
				if err != nil {
					log.Errorf("Error setting key %s, Err: %v", keyName, err)
				}
			}

		case <-stopChan:
			log.Infof("Stop refreshing key: %s", keyName)
			return
		}
	}
}
