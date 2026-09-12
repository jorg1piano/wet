package cmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var portClusterCmd = &cobra.Command{
	Use:   "port-cluster",
	Short: "Move this clone's ports clear of another clone's",
}

var portClusterNextCmd = &cobra.Command{
	Use:   "next",
	Short: "Claim the lowest port cluster that is free on this machine",
	Long: `Shift this clone's declared port bases clear of every other stack running
on this machine, and record the shift in ` + "`.wet.local.yaml`" + `.

Slots already spread ports out inside one checkout: with ` + "`base: 4000`" + ` and
` + "`stride: 10`" + `, the main checkout is on 4000 and its worktrees climb from 4010.
What nothing spreads apart is two *clones* of the same repo — a second clone
starts at slot 0 too, resolves to the same 4000, and the second stack to boot
dies on "port is already allocated".

A cluster is the band of ports one clone owns across all of its worktrees.
` + "`cluster_slots:`" + ` (default 10) is how wide that band is, and ` + "`port_cluster:`" + `
says which band this clone took:

  cluster 0 -> APP_PORT 4000 .. 4090
  cluster 1 -> APP_PORT 4100 .. 4190

` + "`next`" + ` binds every port in every candidate band, lowest cluster first, and
claims the first one where nothing answers. Each port shifts by its own
stride, so a port declared ` + "`stride: 5`" + ` moves 50 per cluster while one on
` + "`stride: 10`" + ` moves 100 — the bands stay disjoint whatever the strides are.

  wet port-cluster next             claim the lowest free cluster
  wet port-cluster next --dry-run   report which one that is, change nothing
  wet port-cluster next --force     re-probe even though this clone has a cluster

The number lands in the ` + "`.wet.local.yaml`" + ` beside the trunk checkout, which
every worktree of this clone reads — a cluster belongs to the clone, not to
one worktree. It is gitignored, so nobody inherits this machine's answer.

Generated env files still hold the old numbers afterwards; re-run
` + "`wet setup`" + ` in each checkout, with its stack down. Setup updates the
recorded ports and regenerates the files. ` + "`wet env pull`" + ` keeps recorded ports.`,
	Args: cobra.NoArgs,
	RunE: runPortClusterNext,
}

var (
	portClusterDryRun bool
	portClusterForce  bool
)

func init() {
	portClusterNextCmd.Flags().BoolVar(&portClusterDryRun, "dry-run", false,
		"report the cluster that would be claimed without writing it")
	portClusterNextCmd.Flags().BoolVar(&portClusterForce, "force", false,
		"probe for a new cluster even when this clone already recorded one")
	portClusterCmd.AddCommand(portClusterNextCmd)
	rootCmd.AddCommand(portClusterCmd)
}

// maxPortClusterProbe caps the search. ClusterFitsPortRange already stops it
// climbing past 65535, but a config with a low base and a small stride has
// hundreds of legal clusters, and walking them all means binding thousands of
// ports to answer a question whose real answer by then is "something on this
// machine is wrong".
const maxPortClusterProbe = 64

func runPortClusterNext(cmd *cobra.Command, args []string) error {
	treePath, err := getRepoRoot()
	if err != nil {
		return err
	}
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return err
	}
	cfg, _, err := LoadWetConfigForWorktree(treePath, repoRoot)
	if err == ErrConfigMissing {
		return fmt.Errorf("`wet port-cluster` requires a .wet.yaml (none at %s or %s)", treePath, repoRoot)
	}
	if err != nil {
		return err
	}
	if len(cfg.Ports) == 0 {
		return errors.New("no ports: declared in .wet.yaml — there is nothing for a cluster to move")
	}

	// A cluster is the clone's, so it is written and read where every
	// checkout of this clone sees it: the machine layer beside the trunk,
	// the same file pool_grow appends emulators to. Writing it into a
	// worktree's own overlay would put that one worktree on a different
	// band from its siblings, which is the collision this command exists to
	// prevent, arranged from the inside.
	localRoot := LocalConfigDir(treePath, repoRoot)
	localPath := filepath.Join(localRoot, wetLocalConfigFileName)

	// Whether this clone already claimed a band is read from the overlay
	// rather than from the merged config, which cannot tell a recorded
	// `port_cluster: 0` from the default.
	claimed, hasClaim, err := overlayPortCluster(localRoot)
	if err != nil {
		return err
	}
	if hasClaim && !portClusterForce {
		// Don't re-probe a clone that already has a band. Its own stack is
		// very likely up, which makes its own ports look taken and would
		// walk it onto a new cluster for no reason — stranding every
		// generated env file and every running container on the old one.
		// `wet worktree setup` declines to re-probe an existing slot for
		// exactly this reason.
		fmt.Printf("Already on port cluster %d (%s).\n", claimed, localPath)
		printClusterPorts(cfg, claimed)
		fmt.Println("\nPass --force to probe for a different one.")
		return nil
	}

	cluster, err := findFreePortCluster(cfg)
	if err != nil {
		return err
	}

	// --force re-probes from 0, so it can just as well land back where it
	// started — the band this clone holds is free and is also the lowest one
	// free. Writing the same number again would be honest but reads as a
	// change nobody made.
	if hasClaim && cluster == claimed {
		fmt.Printf("Port cluster %d is still the lowest free one; staying put (%s).\n", cluster, localPath)
		printClusterPorts(cfg, cluster)
		return nil
	}

	// Say where it is moving FROM whenever there was somewhere to move from:
	// with --force the new cluster can be lower than the old one (a
	// neighbouring clone shut down and freed the band below), and a bare
	// "claimed cluster 0" gives no sign that anything was given up.
	move := ""
	if hasClaim {
		move = fmt.Sprintf(" (was %d)", claimed)
	}
	// Validate what the claim produces before writing it. Nothing found so
	// far should be able to fail this — the search already refuses a cluster
	// that pushes a base past 65535 — which is the point: if the two ever
	// disagree, the overlay must not be the place that records it. A bad
	// number here would fail every later `wet setup` in this clone, in a file
	// nobody thinks to look at, until someone deletes it by hand.
	shifted := *cfg
	shifted.PortCluster = cluster
	if err := shifted.Validate(); err != nil {
		return fmt.Errorf("port cluster %d would make an invalid config (%s left unchanged): %w",
			cluster, localPath, err)
	}

	if portClusterDryRun {
		fmt.Printf("Would claim port cluster %d%s in %s.\n", cluster, move, localPath)
		printClusterPorts(cfg, cluster)
		return nil
	}
	if err := setLocalPortCluster(localRoot, cluster); err != nil {
		return err
	}
	fmt.Printf("Claimed port cluster %d%s (%s).\n", cluster, move, localPath)
	printClusterPorts(cfg, cluster)
	// The claim only moves the numbers wet resolves. Anything already
	// written from the old ones — .env.worktree, every declared output, a
	// running compose stack — still names the band this clone just left.
	fmt.Println("\nRe-run `wet setup` in each checkout of this clone (stack down) to update its recorded ports and generated files.")
	return nil
}

// findFreePortCluster walks clusters from 0 upward and returns the first whose
// whole band is bindable.
//
// It knows nothing about a cluster this clone already holds, deliberately. A
// probe cannot tell "a neighbour is on this band" from "my own stack is on
// this band", so the rule that a claimed band is kept has to be a decision
// about whether to search at all — which is where runPortClusterNext makes
// it, and what --force overrides. Encoding it here instead made --force
// unable to move: the search returned the claimed cluster before ever
// probing it.
func findFreePortCluster(cfg *WetConfig) (int, error) {
	for cluster := 0; cluster <= maxPortClusterProbe; cluster++ {
		// A band whose own ports overlap can never be free, so probing it
		// would silently walk to the next cluster and eventually report
		// "everything is occupied" — blaming the machine for a config bug.
		if a, b, port, bad := cfg.BandConflict(cluster); bad {
			return 0, fmt.Errorf("ports %s and %s both resolve to %d inside cluster %d's band — "+
				"two worktrees of this clone would fight over it; change a base: or a stride: in .wet.yaml",
				a, b, port, cluster)
		}
		if !cfg.ClusterFitsPortRange(cluster) {
			return 0, fmt.Errorf("every port cluster up to %d is occupied, and %d would push a port past 65535 — "+
				"shut down a stack, or lower a base: in .wet.yaml", cluster-1, cluster)
		}
		if cfg.ClusterFree(cluster) {
			return cluster, nil
		}
	}
	return 0, fmt.Errorf("port clusters 0..%d are all occupied — shut down a stack, or raise cluster_slots: "+
		"in .wet.yaml if the bands are too narrow", maxPortClusterProbe)
}

// printClusterPorts shows the band each declared port lands in, so the number
// written to the overlay is checkable against what `docker compose` will bind.
func printClusterPorts(cfg *WetConfig, cluster int) {
	slots := cfg.ClusterSlotCount()
	first := cfg.ResolvePortsIn(cluster, 0)
	last := cfg.ResolvePortsIn(cluster, slots-1)
	nameWidth, portWidth := 0, 0
	for _, p := range first {
		if len(p.Name) > nameWidth {
			nameWidth = len(p.Name)
		}
		if w := len(strconv.Itoa(p.Value)); w > portWidth {
			portWidth = w
		}
	}
	fmt.Printf("  Slots 0-%d of this clone resolve to:\n", slots-1)
	for i, p := range first {
		fmt.Printf("    %-*s  %*d .. %d\n", nameWidth, p.Name, portWidth, p.Value, last[i].Value)
	}
}

// overlayPortCluster reads `port_cluster:` straight out of the machine
// .wet.local.yaml, reporting whether the key is there at all. The merged
// config cannot answer that: an absent key and a recorded `port_cluster: 0`
// both arrive as 0, and they mean opposite things — one clone has claimed
// the bottom band, the other has claimed nothing.
func overlayPortCluster(localDir string) (int, bool, error) {
	root, _, err := readLocalOverlay(filepath.Join(localDir, wetLocalConfigFileName))
	if err != nil {
		return 0, false, err
	}
	node := findMappingValue(root, "port_cluster")
	if node == nil {
		return 0, false, nil
	}
	var cluster int
	if err := node.Decode(&cluster); err != nil {
		return 0, false, fmt.Errorf("%s: port_cluster must be a whole number, not %q",
			filepath.Join(localDir, wetLocalConfigFileName), node.Value)
	}
	return cluster, true, nil
}

// setLocalPortCluster records the claim in the machine overlay, leaving every
// other key, block and comment in the file untouched.
func setLocalPortCluster(localDir string, cluster int) error {
	path := filepath.Join(localDir, wetLocalConfigFileName)
	root, doc, err := readLocalOverlay(path)
	if err != nil {
		return err
	}
	setMappingValue(root, "port_cluster", &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!int",
		Value: fmt.Sprintf("%d", cluster),
	})
	return writeLocalOverlay(path, doc)
}
