package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/application-research/whypfs-core"
	"github.com/cheggaaa/pb/v3"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dsync "github.com/ipfs/go-datastore/sync"
	ipld "github.com/ipfs/go-ipld-format"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multihash"
	"io/ioutil"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
)

const baseURL = "https://bafybeifcghbafml4yrk43m3pvplin4auibnwrdv5v3rnwnovjjpkt6tkju.ipfs.dweb.link/"

type Peer struct {
	ID    string
	Addrs []string
}

// /
// /[
//
//	  {
//	    "ID": "12D3KooWB5HcweB1wdgK8bjfTRHcZdvMFd6ffrn6XqMMyUG7pakP",
//	    "Addrs": ["/dns/bacalhau.dokterbob.net/tcp/4001", "/dns/bacalhau.dokterbob.net/udp/4001/quic"]
//	  }
//	]
//
// /
func main() {

	repo := flag.String("repo", "./whypfs", "path to the repo")
	cidsUrlSource := flag.String("cids-url-source", baseURL, "URL to fetch cids.txt from")
	peers := flag.String("peers", "[{\"ID\":\"12D3KooWB5HcweB1wdgK8bjfTRHcZdvMFd6ffrn6XqMMyUG7pakP\",\"Addrs\":[\"/dns/bacalhau.dokterbob.net/tcp/4001\",\"/dns/bacalhau.dokterbob.net/udp/4001/quic\"]}]", "comma-separated list of peers to connect to")

	// unmarshal the peers string to an array of Peer structs
	peerList := make([]Peer, 0)
	err := json.Unmarshal([]byte(*peers), &peerList)
	if err != nil {
		fmt.Printf("An error occurred while parsing the peers string: %s\n", err)
		return
	}

	// Parse the command-line flags.
	flag.Parse()

	resp, err := http.Get(*cidsUrlSource)
	if err != nil {
		fmt.Printf("An error occurred while fetching cids.txt: %s\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("An error occurred while fetching cids.txt: %s\n", resp.Status)
		return
	}

	cidsBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Error occurred while reading cids.txt: %s\n", err)
		return
	}

	cidsStr := string(cidsBytes)
	cids := strings.Split(strings.TrimSpace(cidsStr), "\n")

	node, err := NewEdgeNode(context.Background(), *repo)
	if err != nil {
		fmt.Printf("Error occurred while creating a new node: %s\n", err)
		return
	}

	// for each peerList, convert it to peer.AddrInfo
	var peerInfos []peer.AddrInfo
	for _, peerItem := range peerList {
		// create peer id
		peerId, err := peer.Decode(peerItem.ID)
		if err != nil {
			fmt.Printf("Error occurred while decoding peer ID: %s\n", err)
			return
		}
		// create multiaddr array
		var multiAddrs []multiaddr.Multiaddr
		for _, addr := range peerItem.Addrs {
			multiAddr, err := multiaddr.NewMultiaddr(addr)
			if err != nil {
				fmt.Printf("Error occurred while creating multiaddr: %s\n", err)
				return
			}
			multiAddrs = append(multiAddrs, multiAddr)
		}

		peerInfo := peer.AddrInfo{
			ID:    peerId,
			Addrs: multiAddrs,
		}
		peerInfos = append(peerInfos, peerInfo)
	}

	// connect
	ConnectToDelegates(context.Background(), *node, peerInfos)

	fmt.Println("List of CIDs:")
	// Number of concurrent goroutines based on the number of CPUs available
	concurrentLimit := runtime.NumCPU()

	// Calculate the batch size per CPU
	batchSizePerCPU := len(cids) / concurrentLimit
	if batchSizePerCPU == 0 {
		batchSizePerCPU = 1 // Ensure there's at least one CID per batch
	}

	// Create a channel to receive errors from goroutines
	results := make(chan error)

	// Create a WaitGroup to wait for all batches to finish
	var allBatchesWG sync.WaitGroup
	allBatchesWG.Add(len(cids) / batchSizePerCPU)

	// Create a semaphore channel to limit the number of goroutines
	sem := make(chan struct{}, concurrentLimit)

	// Divide the CIDs into batches
	batches := splitIntoBatches(cids, batchSizePerCPU)

	// Process each batch sequentially
	for _, batch := range batches {
		fmt.Printf("Processing batch of %d CIDs\n", len(batch))

		// Create a map to store the progress bars for each CID in the current batch
		bars := make(map[string]*pb.ProgressBar)

		// Create a WaitGroup to wait for all goroutines in the current batch to finish
		var wg sync.WaitGroup

		// Launch goroutines
		wg.Add(len(batch))
		for _, cidItem := range batch {
			go fetchCID(cidItem, node, results, &wg, sem, bars[cidItem])
			<-results
		}
		wg.Wait()

		////<-sem // Wait for a free slot in the semaphore channel
		allBatchesWG.Done()
		fmt.Println("Finished processing batch")
	}

	// Wait for all batches to finish before closing the results channel
	allBatchesWG.Wait()
	close(results)

	// Collect errors from the results channel
	for err := range results {
		if err != nil {
			fmt.Printf("Error fetching CID: %s\n", err)
		}
	}

	return
}

func fetchCID(cidItem string, node *whypfs.Node, results chan<- error, wg *sync.WaitGroup, sem chan struct{}, bar *pb.ProgressBar) {
	defer wg.Done()

	// Acquire the semaphore, this will block if the semaphore is full
	sem <- struct{}{}
	defer func() {
		// Release the semaphore after finishing the work
		<-sem
	}()

	cidD, err := cid.Decode(cidItem)
	if err != nil {
		results <- fmt.Errorf("Error decoding cid: %s", err)
		return
	}
	fmt.Print("Fetching CID: ", cidItem)
	nd, errF := node.Get(context.Background(), cidD)
	ndSize, errS := nd.Size()
	if errS != nil {
		results <- fmt.Errorf("error getting cid: %s", errS)
		return
	}
	fmt.Println(" Size: ", ndSize)
	if errF != nil {
		results <- fmt.Errorf("error getting cid: %s", err)
	}
	//dserv := merkledag.NewDAGService(node.Blockservice)
	//cset := cid.NewSet()
	//errW := merkledag.Walk(context.Background(), func(ctx context.Context, c cid.Cid) ([]*ipld.Link, error) {
	//	nodeS, err := dserv.Get(ctx, c)
	//	if err != nil {
	//		return nil, err
	//	}
	//
	//	if c.Type() == cid.Raw {
	//		return nil, nil
	//	}
	//
	//	fmt.Println(nodeS.RawData())
	//	return FilterUnwalkableLinks(nodeS.Links()), nil
	//}, cidD, cset.Visit, merkledag.Concurrent())
	//
	//if errW != nil {
	//	results <- fmt.Errorf("error getting cid: %s", err)
	//}

	results <- nil
}

func FilterUnwalkableLinks(links []*ipld.Link) []*ipld.Link {
	out := make([]*ipld.Link, 0, len(links))

	for _, l := range links {
		if CidIsUnwalkable(l.Cid) {
			continue
		}
		out = append(out, l)
	}

	return out
}

func CidIsUnwalkable(c cid.Cid) bool {
	pref := c.Prefix()
	if pref.MhType == multihash.IDENTITY {
		return true
	}

	if pref.Codec == cid.FilCommitmentSealed || pref.Codec == cid.FilCommitmentUnsealed {
		return true
	}

	return false
}

// splitIntoBatches splits the list of CIDs into batches of the specified batch size.
func splitIntoBatches(cids []string, batchSize int) [][]string {
	var batches [][]string
	for i := 0; i < len(cids); i += batchSize {
		end := i + batchSize
		if end > len(cids) {
			end = len(cids)
		}
		batch := cids[i:end]
		batches = append(batches, batch)
	}
	return batches
}

func NewEdgeNode(ctx context.Context, repo string) (*whypfs.Node, error) {

	// node
	publicIp, err := GetPublicIP()
	newConfig := &whypfs.Config{
		ListenAddrs: []string{
			"/ip4/0.0.0.0/tcp/6745",
			"/ip4/0.0.0.0/udp/6746/quic",
			"/ip4/" + publicIp + "/tcp/6745",
		},
		AnnounceAddrs: []string{
			"/ip4/0.0.0.0/tcp/6745",
			"/ip4/" + publicIp + "/tcp/6745",
		},
	}

	ds := dsync.MutexWrap(datastore.NewMapDatastore())
	//ds, err := levelds.NewDatastore(cfg.Node.DsRepo, nil)
	if err != nil {
		panic(err)
	}
	params := whypfs.NewNodeParams{
		Ctx:       ctx,
		Datastore: ds,
		Repo:      repo,
	}

	params.Config = params.ConfigurationBuilder(newConfig)
	whypfsPeer, err := whypfs.NewNode(params)
	if err != nil {
		panic(err)
	}

	// read the cid text
	return whypfsPeer, nil

}

func GetPublicIP() (string, error) {
	resp, err := http.Get("https://ifconfig.me") // important to get the public ip if possible.
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func ConnectToDelegates(ctx context.Context, node whypfs.Node, peerInfos []peer.AddrInfo) error {

	for _, peerInfo := range peerInfos {
		node.Host.Peerstore().AddAddrs(peerInfo.ID, peerInfo.Addrs, time.Hour)

		if node.Host.Network().Connectedness(peerInfo.ID) != network.Connected {
			if err := node.Host.Connect(ctx, peer.AddrInfo{
				ID: peerInfo.ID,
			}); err != nil {
				return err
			}

			node.Host.ConnManager().Protect(peerInfo.ID, "pinning")
		}
	}

	return nil
}
