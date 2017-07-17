package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"gopkg.in/yaml.v2"
	"html/template"
	"io/ioutil"
	"log"
	"strings"

	"github.com/thoj/go-ircevent"

	"net/http"
	"strconv"
)

var (
	host        = flag.String("host", "irc.hackint.org", "Hostname of the IRC server")
	port        = flag.Int("sslport", 6697, "SSL capable port of the IRC server")
	nickname    = flag.String("nickname", "go-gitlab-irc", "Nickname to assume once connected")
	mappingfile = flag.String("channelmapping", "channelmapping.yml", "Path to channel mapping file.")
	gecos       = flag.String("gecos", "go-gitlab-irc", "Realname to assume once connected")
	cafile      = flag.String("cafile", "hackint-rootca.crt", "Path to the ca file that verifies the server certificate.")
)

type Mapping struct {
	DefaultChannel   string              `yaml:"default"`
	GroupMappings    map[string][]string `yaml:"groups"`
	ExplicitMappings map[string][]string `yaml:"explicit"`
}

func CreateFunctionNotifyFunction(bot *irc.Connection, channelMapping *Mapping) http.HandlerFunc {

	const pushString = "[\x0312{{ .Project.Name }}\x03] {{ .UserName }} pushed {{ .TotalCommits }} new commits to \x0305{{ .Branch }}\x03"
	const commitString = "\x0315{{ .ShortID }}\x03 (\x0303+{{ .AddedFiles }}\x03|\x0308±{{ .ModifiedFiles }}\x03|\x0304-{{ .RemovedFiles }}\x03) - {{ .Message }}"
	const issueString = "[\x0312{{ .Project.Name }}\x03] {{ .User.Name }} created issue \x0308#{{ .Issue.Iid }}\x03: '{{ .Issue.Title }}'"

	pushTemplate, err := template.New("push notification").Parse(pushString)
	if err != nil {
		log.Fatalf("Failed to parse pushEvent template: %v", err)
	}

	commitTemplate, err := template.New("commit notification").Parse(commitString)
	if err != nil {
		log.Fatalf("Failed to parse commitString template: %v", err)
	}

	issueTemplate, err := template.New("issue notification").Parse(issueString)
	if err != nil {
		log.Fatalf("Failed to parse issueEvent template: %v", err)
	}

	return func(wr http.ResponseWriter, req *http.Request) {
		defer req.Body.Close()
		decoder := json.NewDecoder(req.Body)

		var eventType = req.Header["X-Gitlab-Event"][0]

		type Project struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		}

		type User struct {
			Name string `json:"name"`
		}

		type Issue struct {
			Iid         int    `json:"iid"`
			Title       string `json:"title"`
			Description string `json:"description"`
		}

		type Commit struct {
			Id       string   `json:"id"`
			Message  string   `json:"message"`
			Added    []string `"json:"added"`
			Modified []string `"json:"modified"`
			Removed  []string `"json:"removed"`
		}

		type PushEvent struct {
			UserName     string   `json:"user_name"`
			Project      Project  `json:"project"`
			Commits      []Commit `json:"commits"`
			TotalCommits int      `json:"total_commits_count"`
			Branch       string   `json:"ref"`
		}

		type IssueEvent struct {
			User    User    `json:"user"`
			Project Project `json:"project"`
			Issue   Issue   `json:"object_attributes"`
		}

		var buf bytes.Buffer

		switch eventType {

		case "Issue Hook":
			var issueEvent IssueEvent
			if err := decoder.Decode(&issueEvent); err != nil {
				log.Fatal(err)
				return
			}
			err = issueTemplate.Execute(&buf, &issueEvent)

			sendMessage(buf.String(), issueEvent.Project.Name, issueEvent.Project.Namespace, channelMapping, bot)

		case "Push Hook":
			var pushEvent PushEvent
			if err := decoder.Decode(&pushEvent); err != nil {
				log.Println(err)
				return
			}
			pushEvent.Branch = strings.Split(pushEvent.Branch, "/")[2]
			err = pushTemplate.Execute(&buf, &pushEvent)

			sendMessage(buf.String(), pushEvent.Project.Name, pushEvent.Project.Namespace, channelMapping, bot)

			for _, commit := range pushEvent.Commits {
				type CommitContext struct {
					ShortID       string
					Message       string
					AddedFiles    int
					ModifiedFiles int
					RemovedFiles  int
				}

				context := CommitContext{
					ShortID:       commit.Id[0:8],
					Message:       commit.Message,
					AddedFiles:    len(commit.Added),
					ModifiedFiles: len(commit.Modified),
					RemovedFiles:  len(commit.Removed),
				}

				var buf bytes.Buffer
				err = commitTemplate.Execute(&buf, &context)

				if err != nil {
					log.Printf("ERROR: %v", err)
					return
				}
				sendMessage(buf.String(), pushEvent.Project.Name, pushEvent.Project.Namespace, channelMapping, bot)

			}

		default:
			log.Printf("Unknown event: %s", eventType)
		}

	}

}

func getAllChannelNames(channelMapping *Mapping) []string {
	var allNames []string

	// add default channel
	allNames = append(allNames, channelMapping.DefaultChannel)
	// add channels from group mappings
	for _, channelList := range channelMapping.GroupMappings {
		for _, channelName := range channelList {
			allNames = append(allNames, channelName)
		}
	}
	// add channels from explicit mappings
	for _, channelList := range channelMapping.ExplicitMappings {
		for _, channelName := range channelList {
			allNames = append(allNames, channelName)
		}
	}
	return allNames
}

func contains(mapping map[string][]string, entry string) bool {
	for k, _ := range mapping {
		if k == entry {
			return true
		}
	}
	return false
}

func sendMessage(message string, projectName string, namespace string, channelMapping *Mapping, bot *irc.Connection) {
	var channelNames []string
	var full_projectName = namespace + "/" + projectName

	if contains(channelMapping.ExplicitMappings, full_projectName) { // Check if explizit mapping exists
		for _, channelName := range channelMapping.ExplicitMappings[full_projectName] {
			channelNames = append(channelNames, channelName)
		}
	} else if contains(channelMapping.GroupMappings, namespace) { // Check if group mapping exists
		for _, channelName := range channelMapping.GroupMappings[namespace] {
			channelNames = append(channelNames, channelName)
		}
	} else { // Fall back to default channel
		channelNames = append(channelNames, channelMapping.DefaultChannel)
	}

	for _, channelName := range channelNames {
		bot.Privmsg(channelName, message)
	}

}

func main() {
	flag.Parse()

	caCertPool := x509.NewCertPool()
	caCert, err := ioutil.ReadFile(*cafile)
	if err != nil {
		log.Fatal(err)
	}
	caCertPool.AppendCertsFromPEM(caCert)

	// Setup HTTPS client
	tlsConfig := &tls.Config{
		RootCAs: caCertPool,
	}
	irccon := irc.IRC(*nickname, *gecos)

	irccon.Debug = true
	irccon.UseTLS = true
	irccon.TLSConfig = tlsConfig

	channelMapping := new(Mapping)

	yamlFile, err := ioutil.ReadFile(*mappingfile)
	if err != nil {
		log.Fatal(err)
		return
	}

	err = yaml.Unmarshal(yamlFile, channelMapping)
	if err != nil {
		log.Fatal(err)
		return
	}

	RegisterHandlers(irccon, channelMapping)

	var server bytes.Buffer
	server.WriteString(*host)
	server.WriteString(":")
	server.WriteString(strconv.Itoa(*port))

	err = irccon.Connect(server.String())
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		http.HandleFunc("/notify", CreateFunctionNotifyFunction(irccon, channelMapping))
		http.ListenAndServe("127.0.0.1:8084", nil)
	}()

	irccon.Loop()
}

func RegisterHandlers(irccon *irc.Connection, channelMapping *Mapping) {
	irccon.AddCallback("001", func(e *irc.Event) {
		var channelNames = getAllChannelNames(channelMapping)
		for _, channelName := range channelNames {
			log.Printf("Joining %v", channelName)
			irccon.Join(channelName)
		}

	})
	irccon.AddCallback("366", func(e *irc.Event) {})
}
