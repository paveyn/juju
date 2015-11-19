// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"text/template"
	"time"

	goyaml "gopkg.in/yaml.v2"
	"launchpad.net/gnuflag"

	"github.com/juju/cmd"
	"github.com/juju/errors"
	"github.com/juju/juju/api/highavailability"
	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/cmd/envcmd"
	"github.com/juju/juju/juju"
	"github.com/juju/juju/mongo"
	"github.com/juju/juju/network"
	"github.com/juju/loggo"
	"github.com/juju/names"
	"github.com/juju/utils"
)

func (c *upgradeMongoCommand) SetFlags(f *gnuflag.FlagSet) {
	f.BoolVar(&c.local, "local", false, "this is a local provider")
	c.Log.AddFlags(f)
}

func main() {
	Main(os.Args)
}

// Main is the entry point for this plugins.
func Main(args []string) {
	ctx, err := cmd.DefaultContext()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	if err := juju.InitJujuHome(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(2)
	}
	os.Exit(cmd.Main(envcmd.Wrap(&upgradeMongoCommand{}), ctx, args[1:]))
}

const upgradeDoc = `This command upgrades the state server
mongo db from 2.4 to 3.`

var logger = loggo.GetLogger("juju.plugins.upgrademongo")

// MongoUpgradeClient defines the methods
// on the client api that mongo upgrade will call.
type MongoUpgradeClient interface {
	Close() error
	MongoUpgradeMode(mongo.Version) (params.MongoUpgradeResults, error)
}

type upgradeMongoCommand struct {
	envcmd.EnvCommandBase
	Log      cmd.Log
	local    bool
	haClient MongoUpgradeClient
}

func (c *upgradeMongoCommand) Info() *cmd.Info {
	return &cmd.Info{
		Name:    "juju-upgrade-mongo",
		Purpose: "Upgrade from mongo 2.4 to 3.1",
		Args:    "",
		Doc:     upgradeDoc,
	}
}

func mustParseTemplate(templ string) *template.Template {
	t := template.New("").Funcs(template.FuncMap{
		"shquote": utils.ShQuote,
	})
	return template.Must(t.Parse(templ))
}

// runViaJujuSSH will run arbitrary code in the remote machine.
func runViaJujuSSH(machine, script string, stdout, stderr *bytes.Buffer) error {
	cmd := exec.Command("ssh", []string{fmt.Sprintf("ubuntu@%s", machine), "sudo -n bash -c " + utils.ShQuote(script)}...)
	fmt.Println(script)
	cmd.Stderr = stderr
	cmd.Stdout = stdout
	err := cmd.Run()
	if err != nil {
		return errors.Annotatef(err, "ssh command failed: (%q)", stderr.String())
	}
	return nil
}

func bufferPrinter(stdout *bytes.Buffer, closer chan int, verbose bool) {
	for {
		select {
		case <-closer:
			return
		case <-time.After(500 * time.Millisecond):

		}
		line, err := stdout.ReadString(byte('\n'))
		if err == nil || err == io.EOF {
			fmt.Print(line)
		}
		if err != nil && err != io.EOF {
			return
		}

	}
}

// We try to stop/start juju with both systems, safer than
// try a convoluted discovery in bash.
const jujuUpgradeScript = `
/var/lib/juju/tools/machine-{{.MachineNumber}}/jujud upgrade-mongo --series {{.Series}} --machinetag 'machine-{{.MachineNumber}}'
`

type upgradeScriptParams struct {
	MachineNumber string
	Series        string
}

func (c *upgradeMongoCommand) Run(ctx *cmd.Context) error {
	if err := c.Log.Start(ctx); err != nil {
		return err
	}

	migratables, err := c.migratableMachines()
	if err != nil {
		return errors.Annotate(err, "cannot determine status servers")
	}
	var stdout, stderr bytes.Buffer
	var closer chan int
	closer = make(chan int, 1)
	defer func() { closer <- 1 }()
	go bufferPrinter(&stdout, closer, false)

	t := template.New("").Funcs(template.FuncMap{
		"shquote": utils.ShQuote,
	})
	tmpl := template.Must(t.Parse(jujuUpgradeScript))
	var buf bytes.Buffer
	upgradeParams := upgradeScriptParams{
		migratables.master.machine.Id(),
		migratables.master.series,
	}
	err = tmpl.Execute(&buf, upgradeParams)

	if err := runViaJujuSSH(migratables.master.ip.Value, buf.String(), &stdout, &stderr); err != nil {
		return errors.Annotate(err, "migration to mongo 3 unsuccesful")
	}
	return nil
}

type migratable struct {
	machine names.MachineTag
	ip      network.Address
	result  int
	series  string
}

type upgradeMongoParams struct {
	master   migratable
	machines []migratable
}

func (c *upgradeMongoCommand) getHAClient() (MongoUpgradeClient, error) {
	if c.haClient != nil {
		return c.haClient, nil
	}

	root, err := c.NewAPIRoot()
	if err != nil {
		return nil, errors.Annotate(err, "cannot get API connection")
	}

	// NewClient does not return an error, so we'll return nil
	return highavailability.NewClient(root), nil
}

func (c *upgradeMongoCommand) migratableMachines() (upgradeMongoParams, error) {
	haClient, err := c.getHAClient()
	if err != nil {
		return upgradeMongoParams{}, err
	}

	defer haClient.Close()
	// TODO: mongoupgrade mode should return a migratable thinguie.
	results, err := haClient.MongoUpgradeMode(mongo.Mongo30wt)
	if err != nil {
		return upgradeMongoParams{}, errors.Annotate(err, "cannot enter mongo upgrade mode")
	}
	result := upgradeMongoParams{}

	result.master = migratable{
		ip:      results.Master.PublicAddress,
		machine: names.NewMachineTag(results.Master.Tag),
		series:  results.Master.Series,
	}
	for _, member := range results.Members {
		migratableMember := migratable{
			ip:      member.PublicAddress,
			machine: names.NewMachineTag(member.Tag),
			series:  member.Series,
		}
		result.machines = append(result.machines, migratableMember)
	}

	return result, nil

}

func migratableMachinesFromStatus() (*upgradeMongoParams, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("juju", "status", "--format", "yaml")
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	err := cmd.Run()
	if err != nil {
		return nil, errors.Annotate(err, "cannot determine juju state servers")
	}
	var status map[string]interface{}
	err = goyaml.Unmarshal(stdout.Bytes(), &status)
	if err != nil {
		return nil, errors.Annotate(err, "cannot unmarshall status")
	}
	upgradeParams := &upgradeMongoParams{}
	machines := status["machines"].(map[interface{}]interface{})
	for id, machine := range machines {
		m := machine.(map[interface{}]interface{})
		hasVote, isState := m["state-server-member-status"]
		series := m["series"]
		if !isState {
			continue
		}
		tag := names.NewMachineTag(id.(string))
		mi := migratable{
			machine: tag,
			series:  series.(string),
		}
		if hasVote.(string) == "has-vote" {
			upgradeParams.master = mi
		} else {
			upgradeParams.machines = append(upgradeParams.machines, mi)
		}
	}
	return upgradeParams, nil
}

// waitForNotified will wait for all ha members to be notified
// of the impending migration or timeout.
func waitForNotified(addrs []string) error {
	return nil
}

// stopAllMongos stops all the mongo slaves to prevent them
// from falling back when we upgrade the master.
func stopAllMongos(addrs []string) error {
	return nil
}

// recreateReplicas creates replica slaves again from the
// upgraded mongo master.
func recreateReplicas(master string, addrs []string) error {
	return nil
}
