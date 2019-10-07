package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/user"
	"regexp"
	"strings"
	"sync"

	"github.com/Shopify/sarama"
)

type topicArgs struct {
	brokers    string
	tlsCA      string
	tlsCert    string
	tlsCertKey string
	filter     string
	partitions bool
	leaders    bool
	replicas   bool
	verbose    bool
	pretty     bool
	version    sarama.KafkaVersion
}

type topicCmd struct {
	brokers    []string
	tlsCA      string
	tlsCert    string
	tlsCertKey string
	filter     *regexp.Regexp
	partitions bool
	leaders    bool
	replicas   bool
	verbose    bool
	pretty     bool
	version    sarama.KafkaVersion

	client sarama.Client
}

type topic struct {
	Name       string      `json:"name"`
	Partitions []partition `json:"partitions,omitempty"`
}

type partition struct {
	Id           int32   `json:"id"`
	OldestOffset int64   `json:"oldest"`
	NewestOffset int64   `json:"newest"`
	Leader       string  `json:"leader,omitempty"`
	Replicas     []int32 `json:"replicas,omitempty"`
	ISRs         []int32 `json:"isrs,omitempty"`
}

func (cmd *topicCmd) parseFlags(as []string) topicArgs {
	var args topicArgs

	flags := flag.NewFlagSet("topic", flag.ContinueOnError)
	flags.StringVar(&args.brokers, "brokers", "localhost:9092", "Comma separated list of brokers. Port defaults to 9092 when omitted.")
	flags.StringVar(&args.tlsCA, "tlsca", "", "Path to the TLS certificate authority file")
	flags.StringVar(&args.tlsCert, "tlscert", "", "Path to the TLS client certificate file")
	flags.StringVar(&args.tlsCertKey, "tlscertkey", "", "Path to the TLS client certificate key file")
	flags.BoolVar(&args.partitions, "partitions", false, "Include information per partition.")
	flags.BoolVar(&args.leaders, "leaders", false, "Include leader information per partition.")
	flags.BoolVar(&args.replicas, "replicas", false, "Include replica ids per partition.")
	flags.StringVar(&args.filter, "filter", "", "Regex to filter topics by name.")
	flags.BoolVar(&args.verbose, "verbose", false, "More verbose logging to stderr.")
	flags.BoolVar(&args.pretty, "pretty", true, "Control output pretty printing.")
	kafkaVersionFlagVar(flags, &args.version)
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage of topic:")
		flags.PrintDefaults()
		fmt.Fprintln(os.Stderr, topicDocString)
	}

	err := flags.Parse(as)
	if err != nil && strings.Contains(err.Error(), "flag: help requested") {
		os.Exit(0)
	} else if err != nil {
		os.Exit(2)
	}
	if err := setFlagsFromEnv(flags, map[string]string{
		"brokers": "KT_BROKERS",
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	return args
}

func (cmd *topicCmd) parseArgs(as []string) {
	args := cmd.parseFlags(as)
	cmd.brokers = strings.Split(args.brokers, ",")
	for i, b := range cmd.brokers {
		if !strings.Contains(b, ":") {
			cmd.brokers[i] = b + ":9092"
		}
	}

	re, err := regexp.Compile(args.filter)
	if err != nil {
		failf("invalid regex for filter err=%s", err)
	}

	cmd.tlsCA = args.tlsCA
	cmd.tlsCert = args.tlsCert
	cmd.tlsCertKey = args.tlsCertKey
	cmd.filter = re
	cmd.partitions = args.partitions
	cmd.leaders = args.leaders
	cmd.replicas = args.replicas
	cmd.pretty = args.pretty
	cmd.verbose = args.verbose
	cmd.version = args.version
}

func (cmd *topicCmd) connect() {
	var (
		err error
		usr *user.User
		cfg = sarama.NewConfig()
	)

	cfg.Version = cmd.version

	if usr, err = user.Current(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read current user err=%v", err)
	}
	cfg.ClientID = "kt-topic-" + sanitizeUsername(usr.Username)
	if cmd.verbose {
		fmt.Fprintf(os.Stderr, "sarama client configuration %#v\n", cfg)
	}

	tlsConfig, err := setupCerts(cmd.tlsCert, cmd.tlsCA, cmd.tlsCertKey)
	if err != nil {
		failf("failed to setup certificates err=%v", err)
	}
	if tlsConfig != nil {
		cfg.Net.TLS.Enable = true
		cfg.Net.TLS.Config = tlsConfig
	}

	if cmd.client, err = sarama.NewClient(cmd.brokers, cfg); err != nil {
		failf("failed to create client err=%v", err)
	}
}

func (cmd *topicCmd) run(as []string) {
	cmd.parseArgs(as)
	if cmd.verbose {
		sarama.Logger = log.New(os.Stderr, "", log.LstdFlags)
	}

	cmd.connect()
	defer cmd.client.Close()

	all, err := cmd.client.Topics()
	if err != nil {
		failf("failed to read topics err=%v", err)
	}

	topics := []string{}
	for _, a := range all {
		if cmd.filter.MatchString(a) {
			topics = append(topics, a)
		}
	}

	out := newPrinter(cmd.pretty)
	var wg sync.WaitGroup
	for _, topicName := range topics {
		topicName := topicName
		wg.Add(1)
		go func() {
			defer wg.Done()
			topic, err := cmd.readTopic(topicName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to read info for topic %s. err=%v\n", topicName, err)
				return
			}
			out.print(topic)
		}()
	}
	wg.Wait()
}

func (cmd *topicCmd) readTopic(name string) (topic, error) {
	var (
		err error
		ps  []int32
		led *sarama.Broker
		top = topic{Name: name}
	)

	if !cmd.partitions {
		return top, nil
	}

	if ps, err = cmd.client.Partitions(name); err != nil {
		return top, err
	}

	for _, p := range ps {
		np := partition{Id: p}

		if np.OldestOffset, err = cmd.client.GetOffset(name, p, sarama.OffsetOldest); err != nil {
			return top, err
		}

		if np.NewestOffset, err = cmd.client.GetOffset(name, p, sarama.OffsetNewest); err != nil {
			return top, err
		}

		if cmd.leaders {
			if led, err = cmd.client.Leader(name, p); err != nil {
				return top, err
			}
			np.Leader = led.Addr()
		}

		if cmd.replicas {
			if np.Replicas, err = cmd.client.Replicas(name, p); err != nil {
				return top, err
			}

			if np.ISRs, err = cmd.client.InSyncReplicas(name, p); err != nil {
				return top, err
			}
		}

		top.Partitions = append(top.Partitions, np)
	}

	return top, nil
}

var topicDocString = `
The values for -brokers can also be set via the environment variable KT_BROKERS respectively.
The values supplied on the command line win over environment variable values.`
