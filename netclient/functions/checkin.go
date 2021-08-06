package functions

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"

	nodepb "github.com/gravitl/netmaker/grpc"
	"github.com/gravitl/netmaker/models"
	"github.com/gravitl/netmaker/netclient/auth"
	"github.com/gravitl/netmaker/netclient/config"
	"github.com/gravitl/netmaker/netclient/local"
	"github.com/gravitl/netmaker/netclient/wireguard"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	//homedir "github.com/mitchellh/go-homedir"
)

func checkIP(node *models.Node, servercfg config.ServerConfig, cliconf config.ClientConfig, network string) bool {
	ipchange := false
	var err error
	if node.Roaming == "yes" {
		if node.IsLocal == "no" {
			log.Println("Checking to see if public addresses have changed")
			extIP, err := getPublicIP()
			if err != nil {
				log.Println("error encountered checking ip addresses:", err)
			}
			if node.Endpoint != extIP && extIP != "" {
				log.Println("Endpoint has changed from " +
					node.Endpoint + " to " + extIP)
				log.Println("Updating address")
				node.Endpoint = extIP
				ipchange = true
			}
			intIP, err := getPrivateAddr()
			if err != nil {
				log.Println("error encountered checking ip addresses:", err)
			}
			if node.LocalAddress != intIP && intIP != "" {
				log.Println("Local Address has changed from " +
					node.LocalAddress + " to " + intIP)
				log.Println("Updating address")
				node.LocalAddress = intIP
				ipchange = true
			}
		} else {
			log.Println("Checking to see if local addresses have changed")
			localIP, err := getLocalIP(node.LocalRange)
			if err != nil {
				log.Println("error encountered checking ip addresses:", err)
			}
			if node.Endpoint != localIP && localIP != "" {
				log.Println("Endpoint has changed from " +
					node.Endpoint + " to " + localIP)
				log.Println("Updating address")
				node.Endpoint = localIP
				node.LocalAddress = localIP
				ipchange = true
			}
		}
	}
	if ipchange {
		err = config.ModConfig(node)
		if err != nil {
			log.Println("Error:", err)
			return false
		}
		err = wireguard.SetWGConfig(network, false)
		if err != nil {
			log.Println("Error:", err)
			return false
		}
	}
	return ipchange && err == nil
}

func setDNS(node *models.Node, servercfg config.ServerConfig, nodecfg *models.Node) {
	if nodecfg.DNSOn == "yes" {
		log.Println("setting dns")
		ifacename := node.Interface
		nameserver := servercfg.CoreDNSAddr
		network := node.Network
		_ = local.UpdateDNS(ifacename, network, nameserver)
	}
}

/**
 *
 *
 */
func checkNodeActions(node *models.Node, network string, servercfg config.ServerConfig) string {
	if node.Action == models.NODE_UPDATE_KEY {
		err := wireguard.SetWGKeyConfig(network, servercfg.GRPCAddress)
		if err != nil {
			log.Println("Unable to process reset keys request:", err)
			return ""
		}
		return ""
	}
	if node.Action == models.NODE_DELETE {
		err := LeaveNetwork(network)
		if err != nil {
			log.Println("Error:", err)
			return ""
		}
		return models.NODE_DELETE
	}
	return ""
}

/**
 * Pull changes if any (interface refresh)
 * - Save it
 * Check local changes for (ipAddress, publickey, configfile changes) (interface refresh)
 * - Save it
 * - Push it
 * Pull Peers (sync)
 */
func CheckConfig(cliconf config.ClientConfig) error {

	network := cliconf.Network
	cfg, err := config.ReadConfig(network)
	if err != nil {
		return err
	}
	servercfg := cfg.Server

	newNode, err := Pull(network, false)
	if err != nil {
		return err
	}
	if newNode.IsPending == "yes" {
		return errors.New("node is pending")
	}

	actionCompleted := checkNodeActions(newNode, network, servercfg)
	if actionCompleted == models.NODE_DELETE {
		return errors.New("node has been removed")
	}
	// Check if ip changed and push if so
	if checkIP(newNode, servercfg, cliconf, network) {
		err = Push(network)
	}
	return err
}

/**
 * Pull the latest node from server
 * Perform action if necessary
 */
func Pull(network string, manual bool) (*models.Node, error) {
	node := config.GetNode(network)
	cfg, err := config.ReadConfig(network)
	if err != nil {
		return nil, err
	}
	servercfg := cfg.Server
	var header metadata.MD

	if cfg.Node.IPForwarding == "yes" {
		if err = local.SetIPForwarding(); err != nil {
			return nil, err
		}
	}

	var requestOpts grpc.DialOption
	requestOpts = grpc.WithInsecure()
	if cfg.Server.GRPCSSL == "on" {
		h2creds := credentials.NewTLS(&tls.Config{NextProtos: []string{"h2"}})
		requestOpts = grpc.WithTransportCredentials(h2creds)
	}
	conn, err := grpc.Dial(servercfg.GRPCAddress, requestOpts)
	if err != nil {
		log.Println("Cant dial GRPC server:", err)
		return nil, err
	}
	wcclient := nodepb.NewNodeServiceClient(conn)

	ctx, err := auth.SetJWT(wcclient, network)
	if err != nil {
		log.Println("Failed to authenticate:", err)
		return nil, err
	}

	req := &nodepb.Object{
		Data: node.MacAddress + "###" + node.Network,
		Type: nodepb.STRING_TYPE,
	}
	readres, err := wcclient.ReadNode(ctx, req, grpc.Header(&header))
	if err != nil {
		return nil, err
	}
	var resNode models.Node
	if err = json.Unmarshal([]byte(readres.Data), &resNode); err != nil {
		return nil, err
	}
	if resNode.PullChanges == "yes" || manual {
		// check for interface change
		if cfg.Node.Interface != resNode.Interface {
			if err = DeleteInterface(cfg.Node.Interface, cfg.Node.PostDown); err != nil {
				log.Println("could not delete old interface", cfg.Node.Interface)
			}
		}
		if err = config.ModConfig(&resNode); err != nil {
			return nil, err
		}
		if err = wireguard.SetWGConfig(network, false); err != nil {
			return nil, err
		}
		resNode.PullChanges = "no"
		nodeData, err := json.Marshal(&resNode)
		if err != nil {
			return &resNode, err
		}
		req := &nodepb.Object{
			Data:     string(nodeData),
			Type:     nodepb.NODE_TYPE,
			Metadata: "",
		}
		_, err = wcclient.UpdateNode(ctx, req, grpc.Header(&header))
		if err != nil {
			return &resNode, err
		}
	} else {
		if err = wireguard.SetWGConfig(network, true); err != nil {
			return nil, err
		}
	}
	setDNS(&resNode, servercfg, &cfg.Node)

	return &resNode, err
}

func Push(network string) error {
	postnode := config.GetNode(network)
	cfg, err := config.ReadConfig(network)
	if err != nil {
		return err
	}
	servercfg := cfg.Server
	var header metadata.MD

	var wcclient nodepb.NodeServiceClient
	var requestOpts grpc.DialOption
	requestOpts = grpc.WithInsecure()
	if cfg.Server.GRPCSSL == "on" {
		h2creds := credentials.NewTLS(&tls.Config{NextProtos: []string{"h2"}})
		requestOpts = grpc.WithTransportCredentials(h2creds)
	}
	conn, err := grpc.Dial(servercfg.GRPCAddress, requestOpts)
	if err != nil {
		log.Println("Cant dial GRPC server:", err)
		return err
	}
	wcclient = nodepb.NewNodeServiceClient(conn)

	ctx, err := auth.SetJWT(wcclient, network)
	if err != nil {
		log.Println("Failed to authenticate:", err)
		return err
	}
	postnode.SetLastCheckIn()
	nodeData, err := json.Marshal(&postnode)
	if err != nil {
		return err
	}

	req := &nodepb.Object{
		Data:     string(nodeData),
		Type:     nodepb.NODE_TYPE,
		Metadata: "",
	}
	data, err := wcclient.UpdateNode(ctx, req, grpc.Header(&header))
	if err != nil {
		return err
	}
	err = json.Unmarshal([]byte(data.Data), &postnode)
	if err != nil {
		return err
	}
	err = config.ModConfig(&postnode)
	return err
}
