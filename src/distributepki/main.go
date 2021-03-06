package main

import (
	"distributepki/clientapi"
	"distributepki/keystore"
	"distributepki/util"
	"encoding/gob"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"pbft"

	"github.com/coreos/pkg/capnslog"
)

var (
	log = capnslog.NewPackageLogger("github.com/sydli/distributePKI", "main")
)

func logFatal(e error) {
	if e != nil {
		log.Fatal(e)
	}
}

func LoadConfig(filename string) pbft.ClusterConfig {
	log.Infof("Reading cluster configuration from %s...", filename)
	configData, err := ioutil.ReadFile(filename)
	logFatal(err)
	var config pbft.ClusterConfig
	err = json.Unmarshal(configData, &config)
	logFatal(err)
	return config
}

func LoadConfigSubset(filename string, num int) pbft.ClusterConfig {
	config := LoadConfig(filename)
	config.Nodes = config.Nodes[0:num]
	return pbft.ClusterConfig{
		Nodes:            config.Nodes[0:num],
		AuthorityKeyFile: config.AuthorityKeyFile,
		Endpoint:         config.Endpoint,
	}
}

func LoadInitialKeys(filename string, config *pbft.ClusterConfig) map[string]string {
	log.Infof("Reading initial keys from %s...", filename)

	initialKeyTable := make(map[string]string)
	// TODO: (jlwatson) fix this shiiiiii
	/*
		keyData, err := ioutil.ReadFile(filename)
		logFatal(err)

		var initialKeys []pbft.KeyPair
		err = json.Unmarshal(keyData, &initialKeys)
		logFatal(err)

		for _, n := range config.Nodes {
			initialKeys = append(initialKeys, pbft.KeyPair{Key: n.Key, Alias: util.GetHostname(n.Host, n.Port)})
		}

		for _, kp := range initialKeys {
			initialKeyTable[string(kp.Alias)] = string(kp.Key)
			log.Debugf("    %v => %v", kp.Alias, kp.Key)
		}
	*/
	return initialKeyTable
}

func main() {

	configFile := flag.String("config", "cluster.json", "PBFT configuration file")
	cluster := flag.Bool("cluster", false, "Bootstrap entire cluster")
	num := flag.Int("num", 0, "Subset of cluster")
	debug := flag.Bool("debug", false, "with cluster flag, enables debugging. without cluster flag, starts debugging repl")
	id := flag.Int("id", 1, "Node ID to start")
	keystoreFile := flag.String("keys", "keys.json", "Initial keys in store")
	flag.Parse()

	// Register Gob types
	gob.Register(clientapi.Create{})
	gob.Register(clientapi.Update{})
	gob.Register(clientapi.Lookup{})
	var config pbft.ClusterConfig
	if *num == 0 {
		config = LoadConfig(*configFile)
	} else {
		config = LoadConfigSubset(*configFile, *num)
	}
	initialKeyTable := LoadInitialKeys(*keystoreFile, &config)

	if *cluster {
		StartCluster(&initialKeyTable, &config, make(chan struct{}), *debug)
	} else if *debug {
		StartDebugRepl(&config)
	} else {
		StartNode(pbft.NodeId(*id), &initialKeyTable, &config)
	}
}

func StartCluster(initialKeyTable *map[string]string, cluster *pbft.ClusterConfig, shutdown chan struct{}, debug bool) {
	var nodeProcesses []*exec.Cmd
	for _, n := range cluster.Nodes {
		id := n.Id
		if debug {
			// TODO (sydli): Take debug flag into account
		}
		cmd := exec.Command("./distributepki", "-id", fmt.Sprintf("%d", id), "-num",
			fmt.Sprintf("%d", len(cluster.Nodes)))
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Dir = "."
		err := cmd.Start()
		if err != nil {
			log.Fatal(err)
			continue
		}
		nodeProcesses = append(nodeProcesses, cmd)
	}
	// If we get Ctrl+C, kill all subprocesses
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	select {
	case <-c:
	case <-shutdown:
	}
	for i, cmd := range nodeProcesses {
		log.Infof("Kill process %d", i)
		cmd.Process.Kill()
	}
}

func StartNode(id pbft.NodeId, initialKeyTable *map[string]string, cluster *pbft.ClusterConfig) {
	var thisNode pbft.NodeConfig
	for _, n := range cluster.Nodes {
		if n.Id == id {
			thisNode = n
		}
	}

	store := keystore.NewKeystore(initialKeyTable)

	log.Infof("Starting node %d (%s)...", id, util.GetHostname(thisNode.Host, thisNode.Port))
	node := SpawnKeyNode(thisNode, cluster, store)
	if node == nil {
		log.Fatalf("Node %d failed to start.", id)
		return
	}
	log.Infof("Node %d started successfully!", id)

	node.StartClientServer(thisNode.ClientPort)

	<-node.consensusNode.Failure()
}
