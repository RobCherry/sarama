package kafka

import k "sarama/protocol"

import (
	"sort"
	"sync"
)

// Client is a generic Kafka client. It manages connections to one or more Kafka brokers.
// You MUST call Close() on a client to avoid leaks, it will not be garbage-collected
// automatically when it passes out of scope. A single client can be safely shared by
// multiple concurrent Producers and Consumers.
type Client struct {
	id      string                     // client id for broker requests
	brokers map[int32]*k.Broker        // maps broker ids to brokers
	leaders map[string]map[int32]int32 // maps topics to partition ids to broker ids
	lock    sync.RWMutex               // protects access to the maps, only one since they're always accessed together
}

// NewClient creates a new Client with the given client ID. It connects to the broker at the given
// host:port address, and uses that broker to automatically fetch metadata on the rest of the kafka cluster.
// If metadata cannot be retrieved (even if the connection otherwise succeeds) then the client is not created.
func NewClient(id string, host string, port int32) (client *Client, err error) {
	tmp := k.NewBroker(host, port)
	err = tmp.Connect()
	if err != nil {
		return nil, err
	}

	client = new(Client)
	client.id = id

	client.brokers = make(map[int32]*k.Broker)
	client.leaders = make(map[string]map[int32]int32)

	// add it temporarily with an invalid ID so that refreshTopics can find it
	client.brokers[-1] = tmp

	// do an initial fetch of all cluster metadata by specifing an empty list of topics
	err = client.refreshTopics(make([]string, 0))
	if err != nil {
		client.Close() // this closes tmp, since it's still in the brokers hash
		return nil, err
	}

	// now remove our tmp broker - the successful metadata request will have returned it
	// with a valid ID, so it will already be in the hash somewhere else and we don't need
	// the incomplete tmp one anymore
	go client.brokers[-1].Close()
	delete(client.brokers, -1)

	return client, nil
}

// Close shuts down all broker connections managed by this client. It is required to call this function before
// a client object passes out of scope, as it will otherwise leak memory. You must close any Producers or Consumers
// using a client before you close the client.
func (client *Client) Close() {
	client.lock.Lock()
	defer client.lock.Unlock()

	for _, broker := range client.brokers {
		go broker.Close()
	}
	client.brokers = nil
	client.leaders = nil
}

func (client *Client) leader(topic string, partition_id int32) (*k.Broker, error) {
	leader := client.cachedLeader(topic, partition_id)

	if leader == nil {
		err := client.refreshTopic(topic)
		if err != nil {
			return nil, err
		}

		leader = client.cachedLeader(topic, partition_id)
	}

	if leader == nil {
		return nil, k.UNKNOWN_TOPIC_OR_PARTITION
	}

	return leader, nil
}

func (client *Client) partitions(topic string) ([]int32, error) {
	partitions := client.cachedPartitions(topic)

	if partitions == nil {
		err := client.refreshTopic(topic)
		if err != nil {
			return nil, err
		}

		partitions = client.cachedPartitions(topic)
	}

	if partitions == nil {
		return nil, NoSuchTopic
	}

	return partitions, nil
}

func (client *Client) cachedLeader(topic string, partition_id int32) *k.Broker {
	client.lock.RLock()
	defer client.lock.RUnlock()

	partitions := client.leaders[topic]
	if partitions != nil {
		leader := partitions[partition_id]
		if leader == -1 {
			return nil
		} else {
			return client.brokers[leader]
		}
	}

	return nil
}

func (client *Client) any() *k.Broker {
	client.lock.RLock()
	defer client.lock.RUnlock()

	for _, broker := range client.brokers {
		return broker
	}

	return nil
}

func (client *Client) cachedPartitions(topic string) []int32 {
	client.lock.RLock()
	defer client.lock.RUnlock()

	partitions := client.leaders[topic]
	if partitions == nil {
		return nil
	}

	ret := make([]int32, len(partitions))
	for id, _ := range partitions {
		ret = append(ret, id)
	}

	sort.Sort(int32Slice(ret))
	return ret
}

func (client *Client) update(data *k.MetadataResponse) error {
	// First discard brokers that we already know about. This avoids bouncing TCP connections,
	// and especially avoids closing valid connections out from under other code which may be trying
	// to use them. We only need a read-lock for this.
	var newBrokers []*k.Broker
	client.lock.RLock()
	for _, broker := range data.Brokers {
		if !broker.Equals(client.brokers[broker.ID()]) {
			newBrokers = append(newBrokers, broker)
		}
	}
	client.lock.RUnlock()

	// connect to the brokers before taking the write lock, as this can take a while
	// to timeout if one of them isn't reachable
	for _, broker := range newBrokers {
		err := broker.Connect()
		if err != nil {
			return err
		}
	}

	client.lock.Lock()
	defer client.lock.Unlock()

	for _, broker := range newBrokers {
		if client.brokers[broker.ID()] != nil {
			go client.brokers[broker.ID()].Close()
		}
		client.brokers[broker.ID()] = broker
	}

	for _, topic := range data.Topics {
		if topic.Err != k.NO_ERROR {
			return topic.Err
		}
		client.leaders[topic.Name] = make(map[int32]int32, len(topic.Partitions))
		for _, partition := range topic.Partitions {
			if partition.Err != k.NO_ERROR {
				return partition.Err
			}
			client.leaders[topic.Name][partition.Id] = partition.Leader
		}
	}

	return nil
}

func (client *Client) refreshTopics(topics []string) error {
	for broker := client.any(); broker != nil; broker = client.any() {
		response, err := broker.GetMetadata(client.id, &k.MetadataRequest{Topics: topics})

		switch err.(type) {
		case nil:
			// valid response, use it
			return client.update(response)
		case k.EncodingError:
			// didn't even send, return the error
			return err
		}

		// some other error, remove that broker and try again
		client.lock.Lock()
		delete(client.brokers, broker.ID())
		go broker.Close()
		client.lock.Unlock()
	}

	return OutOfBrokers
}

func (client *Client) refreshTopic(topic string) error {
	tmp := make([]string, 1)
	tmp[0] = topic
	return client.refreshTopics(tmp)
}
