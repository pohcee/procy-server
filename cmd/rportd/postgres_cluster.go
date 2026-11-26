package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/pohcee/procy-server/server/chconfig"
	"github.com/pohcee/procy-server/server/peer"
)

var (
	clusterCmd = &cobra.Command{
		Use:   "cluster",
		Short: "cluster helpers (Postgres storage)",
		Long:  "Helpers for servers sharing a Postgres database as a cluster.",
	}
	clusterCACmd = &cobra.Command{
		Use:   "ca",
		Short: "create a CA for peer certificates (development and tests)",
		Long: "Create a self-signed CA certificate and key that issue the certificates processes of a cluster " +
			"authenticate peer calls with. For development and tests: production certificates are issued by the " +
			"deployment (a CA service or a CA key in a secret store), short-lived.",
		Example: "rportd cluster ca --cert ca.crt --key ca.key",
		Args:    cobra.NoArgs,
		Run: func(*cobra.Command, []string) {
			if err := runClusterCA(clusterCAOpts); err != nil {
				log.Fatal(err)
			}
		},
	}
	clusterCertCmd = &cobra.Command{
		Use:   "cert",
		Short: "issue a peer certificate for a node (development and tests)",
		Long: "Issue a certificate and key for a process of a cluster, signed by a CA key. The certificate binds " +
			"the node id and the process roles (api: makes peer calls; device: answers them; a process with " +
			"[storage] role = \"all\" needs both) as URIs spiffe://<trust domain>/role/<role>/node/<node id>.",
		Example: "rportd cluster cert --ca-cert ca.crt --ca-key ca.key --node node-1 --role device " +
			"--cert node-1-device.crt --key node-1-device.key",
		Args: cobra.NoArgs,
		Run: func(*cobra.Command, []string) {
			if err := runClusterCert(clusterCertOpts); err != nil {
				log.Fatal(err)
			}
		},
	}

	clusterCAOpts   = &clusterCertFlags{}
	clusterCertOpts = &clusterCertFlags{}
)

type clusterCertFlags struct {
	caCert, caKey, cert, key string
	node, trustDomain        string
	roles, hosts             []string
	validity                 time.Duration
}

func writeNew(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func runClusterCA(o *clusterCertFlags) error {
	if o.cert == "" || o.key == "" {
		return errors.New("--cert and --key are required")
	}
	cert, key, err := peer.NewCA("rport cluster CA", o.validity)
	if err != nil {
		return err
	}
	if err := writeNew(o.cert, cert, 0o644); err != nil {
		return err
	}
	if err := writeNew(o.key, key, 0o600); err != nil {
		return err
	}
	fmt.Printf("CA certificate %s, key %s (valid for %s)\n", o.cert, o.key, o.validity)
	return nil
}

func runClusterCert(o *clusterCertFlags) error {
	for name, v := range map[string]string{"--ca-cert": o.caCert, "--ca-key": o.caKey, "--node": o.node, "--cert": o.cert, "--key": o.key} {
		if v == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	caCert, err := os.ReadFile(o.caCert)
	if err != nil {
		return err
	}
	caKey, err := os.ReadFile(o.caKey)
	if err != nil {
		return err
	}
	var roles []peer.Role
	for _, r := range o.roles {
		for _, p := range strings.Split(r, ",") {
			if p = strings.TrimSpace(p); p != "" {
				roles = append(roles, peer.Role(p))
			}
		}
	}
	cert, key, err := peer.IssueNodeCert(caCert, caKey, peer.NodeCertRequest{
		TrustDomain: o.trustDomain, NodeID: o.node, Roles: roles, Hosts: o.hosts, Validity: o.validity,
	})
	if err != nil {
		return err
	}
	if err := writeNew(o.cert, cert, 0o644); err != nil {
		return err
	}
	if err := writeNew(o.key, key, 0o600); err != nil {
		return err
	}
	fmt.Printf("certificate %s, key %s for node %s, roles %v (valid for %s)\n", o.cert, o.key, o.node, roles, o.validity)
	return nil
}

// in a file named after main.go: RootCmd is made in main.go's init
func init() {
	RootCmd.AddCommand(clusterCmd)
	clusterCmd.AddCommand(clusterCACmd, clusterCertCmd)

	clusterCACmd.Flags().StringVar(&clusterCAOpts.cert, "cert", "", "file to write the CA certificate to (must not exist)")
	clusterCACmd.Flags().StringVar(&clusterCAOpts.key, "key", "", "file to write the CA key to (must not exist)")
	clusterCACmd.Flags().DurationVar(&clusterCAOpts.validity, "validity", 365*24*time.Hour, "how long the CA is valid")

	f := clusterCertCmd.Flags()
	f.StringVar(&clusterCertOpts.caCert, "ca-cert", "", "CA certificate (PEM)")
	f.StringVar(&clusterCertOpts.caKey, "ca-key", "", "CA key (PEM)")
	f.StringVar(&clusterCertOpts.node, "node", "", "node id ([cluster] node_id)")
	f.StringSliceVar(&clusterCertOpts.roles, "role", []string{string(peer.RoleDevice)}, "roles: api, device or both (api,device)")
	f.StringVar(&clusterCertOpts.trustDomain, "trust-domain", chconfig.DefaultTrustDomain, "trust domain ([cluster] trust_domain)")
	f.StringSliceVar(&clusterCertOpts.hosts, "host", nil, "extra DNS names or IP addresses")
	f.StringVar(&clusterCertOpts.cert, "cert", "", "file to write the certificate to (must not exist)")
	f.StringVar(&clusterCertOpts.key, "key", "", "file to write the key to (must not exist)")
	f.DurationVar(&clusterCertOpts.validity, "validity", 24*time.Hour, "how long the certificate is valid")

	clusterCmd.SetUsageFunc((&cobra.Command{}).UsageFunc())
}
