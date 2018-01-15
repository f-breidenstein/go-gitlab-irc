package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"gopkg.in/yaml.v2"
	"html"
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

	const pushCompareString = "[\x0312{{ .Project.Name }}\x03] {{ .UserName }} pushed {{ .TotalCommits }} commits to \x0305{{ .Branch }}\x03 {{ .Project.WebURL }}/compare/{{ .BeforeCommit }}...{{ .AfterCommit }}"
	const pushCommitLogString = "[\x0312{{ .Project.Name }}\x03] {{ .UserName }} pushed {{ .TotalCommits }} commits to \x0305{{ .Branch }}\x03 {{ .Project.WebURL }}/commits/{{ .Branch }}"
	const branchCreateString = "[\x0312{{ .Project.Name }}\x03] {{ .UserName }} created the branch \x0305{{ .Branch }}\x03"
	const branchDeleteString = "[\x0312{{ .Project.Name }}\x03] {{ .UserName }} deleted the branch \x0305{{ .Branch }}\x03"
	const commitString = "\x0315{{ .ShortID }}\x03 (\x0303+{{ .AddedFiles }}\x03|\x0308±{{ .ModifiedFiles }}\x03|\x0304-{{ .RemovedFiles }}\x03) \x0306{{ .Author.Name }}\x03: {{ .Message }}"
	const issueString = "[\x0312{{ .Project.Name }}\x03] {{ .User.Name }} {{ .Issue.Action }} issue \x0308#{{ .Issue.Iid }}\x03: {{ .Issue.Title }} {{ .Issue.URL }}"
	const mergeString = "[\x0312{{ .Project.Name }}\x03] {{ .User.Name }} {{ .Merge.Action }} merge request \x0308#{{ .Merge.Iid }}\x03: {{ .Merge.Title }} {{ .Merge.URL }}"
	const pipelineCreateString = "[\x0312{{ .Project.Name }}\x03] Pipeline for commit {{ .Pipeline.Commit }} {{ .Pipeline.Status }} {{ .Project.WebURL }}/pipelines/{{ .Pipeline.Id }}"
	const pipelineCompleteString = "[\x0312{{ .Project.Name }}\x03] Pipeline for commit {{ .Pipeline.Commit }} {{ .Pipeline.Status }} in {{ .Pipeline.Duration }} seconds {{ .Project.WebURL }}/pipelines/{{ .Pipeline.Id }}"
	const jobCompleteString = "[\x0312{{ .Repository.Name }}\x03] Job \x0308{{ .Name }}\x03 for commit {{ .Commit }} {{ .Status }} in {{ .Duration }} seconds {{ .Repository.Homepage }}/-/jobs/{{ .Id }}"

	JobStatus := map[string]string{
		"pending": "is \x0315pending\x03",
		"created": "was \x0315created\x03",
		"running": "is \x0307running\x03",
		"failed":  "has \x0304failed\x03",
		"success": "has \x0303succeded\x03",
	}

	HookActions := map[string]string{
		"open":   "opened",
		"update": "updated",
		"close":  "closed",
		"reopen": "reopened",
		"merge":  "merged",
	}

	const NullCommit = "0000000000000000000000000000000000000000"

	pushCompareTemplate, err := template.New("push notification").Parse(pushCompareString)
	if err != nil {
		log.Fatalf("Failed to parse pushCompare template: %v", err)
	}

	pushCommitLogTemplate, err := template.New("push to new branch notification").Parse(pushCommitLogString)
	if err != nil {
		log.Fatalf("Failed to parse pushCommitLog template: %v", err)
	}

	branchCreateTemplate, err := template.New("branch creat notification").Parse(branchCreateString)
	if err != nil {
		log.Fatalf("Failed to parse branchDelete template: %v", err)
	}

	branchDeleteTemplate, err := template.New("branch delete notification").Parse(branchDeleteString)
	if err != nil {
		log.Fatalf("Failed to parse branchDelete template: %v", err)
	}

	commitTemplate, err := template.New("commit notification").Parse(commitString)
	if err != nil {
		log.Fatalf("Failed to parse commitString template: %v", err)
	}

	issueTemplate, err := template.New("issue notification").Parse(issueString)
	if err != nil {
		log.Fatalf("Failed to parse issueEvent template: %v", err)
	}

	mergeTemplate, err := template.New("merge notification").Parse(mergeString)
	if err != nil {
		log.Fatalf("Failed to parse mergeEvent template: %v", err)
	}

	pipelineCreateTemplate, err := template.New("pipeline create notification").Parse(pipelineCreateString)
	if err != nil {
		log.Fatalf("Failed to parse pipelineCreateEvent template: %v", err)
	}

	pipelineCompleteTemplate, err := template.New("pipeline complete notification").Parse(pipelineCompleteString)
	if err != nil {
		log.Fatalf("Failed to parse pipelineCompleteEvent template: %v", err)
	}

	jobCompleteTemplate, err := template.New("job complete notification").Parse(jobCompleteString)
	if err != nil {
		log.Fatalf("Failed to parse jobCompleteEvent template: %v", err)
	}

	return func(wr http.ResponseWriter, req *http.Request) {
		defer req.Body.Close()
		decoder := json.NewDecoder(req.Body)

		var eventType = req.Header["X-Gitlab-Event"][0]

		type Project struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			WebURL    string `json:"web_url"`
		}

		type User struct {
			Name string `json:"name"`
		}

		type Issue struct {
			Iid         int    `json:"iid"`
			Action      string `json:"action"`
			Title       string `json:"title"`
			Description string `json:"description"`
			URL         string `json:"url"`
		}

		type Author struct {
			Name string `json:"name"`
		}

		type Commit struct {
			Id       string   `json:"id"`
			Message  string   `json:"message"`
			Added    []string `json:"added"`
			Modified []string `json:"modified"`
			Removed  []string `json:"removed"`
			Author   Author   `json:"author"`
		}

		type PushEvent struct {
			UserName     string   `json:"user_name"`
			BeforeCommit string   `json:"before"`
			AfterCommit  string   `json:"after"`
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

		type Merge struct {
			Iid    int    `json:"iid"`
			Action string `json:"action"`
			Title  string `json:"title"`
			URL    string `json:"url"`
		}

		type MergeEvent struct {
			User    User    `json:"user"`
			Project Project `json:"project"`
			Merge   Merge   `json:"object_attributes"`
		}

		type Pipeline struct {
			Id       int     `json:"id"`
			Commit   string  `json:"sha"`
			Status   string  `json:"status"`
			Duration float64 `json:"duration"`
		}

		type PipelineEvent struct {
			Pipeline Pipeline `json:"object_attributes"`
			Project  Project  `json:"project"`
		}

		type Repository struct {
			Name     string `json:"name"`
			Homepage string `json:"homepage"`
			URL      string `json:"url"`
		}

		type JobEvent struct {
			Id         int        `json:"build_id"`
			Name       string     `json:"build_name"`
			Status     string     `json:"build_status"`
			Duration   float64    `json:"build_duration"`
			Commit     string     `json:"sha"`
			Repository Repository `json:"repository"`
		}

		var buf bytes.Buffer

		switch eventType {

		case "Pipeline Hook":
			log.Printf("Got a Hook for a Pipeline Event")
			var pipelineEvent PipelineEvent
			if err := decoder.Decode(&pipelineEvent); err != nil {
				log.Fatal(err)
				return
			}

			// pending / running
			if pipelineEvent.Pipeline.Status == "pending" {
				log.Printf("Skipping noisy pipeline event with status: %s", pipelineEvent.Pipeline.Status)
				return
			}

			// shorten commit id
			pipelineEvent.Pipeline.Commit = pipelineEvent.Pipeline.Commit[0:7]

			if pipelineEvent.Pipeline.Status == "running" {
				// colorize status
				pipelineEvent.Pipeline.Status = JobStatus[pipelineEvent.Pipeline.Status]

				err = pipelineCreateTemplate.Execute(&buf, &pipelineEvent)
				sendMessage(buf.String(), pipelineEvent.Project.Name, pipelineEvent.Project.Namespace, channelMapping, bot)

			} else if pipelineEvent.Pipeline.Status == "success" || pipelineEvent.Pipeline.Status == "failed" {
				// colorize status
				pipelineEvent.Pipeline.Status = JobStatus[pipelineEvent.Pipeline.Status]

				err = pipelineCompleteTemplate.Execute(&buf, &pipelineEvent)
				sendMessage(buf.String(), pipelineEvent.Project.Name, pipelineEvent.Project.Namespace, channelMapping, bot)
			}


		case "Job Hook":
			log.Printf("Got a Hook for a Job Event")
			var jobEvent JobEvent
			if err := decoder.Decode(&jobEvent); err != nil {
				log.Fatal(err)
				return
			}

			if jobEvent.Status != "success" && jobEvent.Status != "failed" {
				log.Printf("Skipping noisy job event with status: %s", jobEvent.Status)
				return
			}

			// shorten commit id
			jobEvent.Commit = jobEvent.Commit[0:7]

			// parse namespace from Git URL
			namespace := strings.Split(strings.Split(jobEvent.Repository.URL, ":")[1], "/")[0]

			// colorize status
			jobEvent.Status = JobStatus[jobEvent.Status]

			err = jobCompleteTemplate.Execute(&buf, &jobEvent)
			sendMessage(buf.String(), jobEvent.Repository.Name, namespace, channelMapping, bot)

		case "Merge Request Hook", "Merge Request Event":
			log.Printf("Got Hook for a Merge Request")
			var mergeEvent MergeEvent
			if err := decoder.Decode(&mergeEvent); err != nil {
				log.Fatal(err)
				return
			}

			mergeEvent.Merge.Action = HookActions[mergeEvent.Merge.Action]

			err = mergeTemplate.Execute(&buf, &mergeEvent)

			sendMessage(buf.String(), mergeEvent.Project.Name, mergeEvent.Project.Namespace, channelMapping, bot)

		case "Issue Hook", "Issue Event":
			log.Printf("Got Hook for an Issue")
			var issueEvent IssueEvent
			if err := decoder.Decode(&issueEvent); err != nil {
				log.Fatal(err)
				return
			}

			issueEvent.Issue.Action = HookActions[issueEvent.Issue.Action]

			err = issueTemplate.Execute(&buf, &issueEvent)

			sendMessage(buf.String(), issueEvent.Project.Name, issueEvent.Project.Namespace, channelMapping, bot)

		case "Push Hook", "Push Event":
			log.Printf("Got Hook for a Push Event")
			var pushEvent PushEvent
			if err := decoder.Decode(&pushEvent); err != nil {
				log.Println(err)
				return
			}

			pushEvent.Branch = strings.Split(pushEvent.Branch, "/")[2]

			if pushEvent.AfterCommit == NullCommit {
				// Branch was deleted
				var buf bytes.Buffer
				err = branchDeleteTemplate.Execute(&buf, &pushEvent)
				sendMessage(buf.String(), pushEvent.Project.Name, pushEvent.Project.Namespace, channelMapping, bot)
			} else {
				if pushEvent.BeforeCommit == NullCommit {
					// Branch was created
					var buf bytes.Buffer
					err = branchCreateTemplate.Execute(&buf, &pushEvent)
					sendMessage(buf.String(), pushEvent.Project.Name, pushEvent.Project.Namespace, channelMapping, bot)
				}

				if pushEvent.TotalCommits > 0 {
					// when the beforeCommit does not exist, we can't link to a compare without skipping the first commit
					var buf bytes.Buffer
					if pushEvent.BeforeCommit == NullCommit {
						err = pushCommitLogTemplate.Execute(&buf, &pushEvent)
					} else {
						pushEvent.BeforeCommit = pushEvent.BeforeCommit[0:7]
						pushEvent.AfterCommit = pushEvent.AfterCommit[0:7]
						err = pushCompareTemplate.Execute(&buf, &pushEvent)
					}

					sendMessage(buf.String(), pushEvent.Project.Name, pushEvent.Project.Namespace, channelMapping, bot)

					// Limit number of commit meessages to 3
					if pushEvent.TotalCommits > 3 {
						pushEvent.Commits = pushEvent.Commits[0:3]
					}

					for _, commit := range pushEvent.Commits {
						type CommitContext struct {
							ShortID       string
							Message       string
							Author        Author
							AddedFiles    int
							ModifiedFiles int
							RemovedFiles  int
						}

						context := CommitContext{
							ShortID:       commit.Id[0:7],
							Message:       html.UnescapeString(commit.Message),
							Author:        commit.Author,
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

					if pushEvent.TotalCommits > 3 {
						var message = fmt.Sprintf("and %d more commits.", pushEvent.TotalCommits-3)
						sendMessage(message, pushEvent.Project.Name, pushEvent.Project.Namespace, channelMapping, bot)
					}
				}
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
		http.ListenAndServe(":8084", nil)
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
