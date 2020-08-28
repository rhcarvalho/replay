package main

import (
	"bytes"
	"flag"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"strings"
	"time"
	"encoding/json"
)

var httpClient = &http.Client{}

var (
	all         *bool
	id          *string
	ignore      *bool
	database    string
	db			*string
	js			*string
	py			*string
	dsn         DSN
	SENTRY_URL  string
	exists      bool
	projectDSNs map[string]*DSN
)

type DSN struct {
	host      string
	rawurl    string
	key       string
	projectId string
}

func parseDSN(rawurl string) *DSN {
	fmt.Println("> rawlurl", rawurl)

	// TODO support for http vs. https 7: vs 8:
	key := strings.Split(rawurl, "@")[0][7:]

	uri, err := url.Parse(rawurl)
	if err != nil {
		panic(err)
	}
	idx := strings.LastIndex(uri.Path, "/")
	if idx == -1 {
		log.Fatal("missing projectId in dsn")
	}
	projectId := uri.Path[idx+1:]

	var host string
	if strings.Contains(rawurl, "ingest.sentry.io") {
		host = "ingest.sentry.io"
	}
	if strings.Contains(rawurl, "@localhost:") {
		host = "localhost:9000"
	}
	if host == "" {
		log.Fatal("missing host")
	}
	// if len(key) < 31 || len(key) > 32 {
	// 	log.Fatal("bad key length")
	// }
	if projectId == "" {
		log.Fatal("missing project Id")
	}
	fmt.Printf("> DSN { host: %s, projectId: %s }\n", host, projectId)
	return &DSN{
		host,
		rawurl,
		key,
		projectId,
	}
}

func (d DSN) storeEndpoint() string {
	var fullurl string
	if d.host == "ingest.sentry.io" {
		fullurl = fmt.Sprint("https://", d.host, "/api/", d.projectId, "/store/?sentry_key=", d.key, "&sentry_version=7")
	}
	if d.host == "localhost:9000" {
		fullurl = fmt.Sprint("http://", d.host, "/api/", d.projectId, "/store/?sentry_key=", d.key, "&sentry_version=7")
	}
	if fullurl == "" {
		log.Fatal("problem with fullurl")
	}
	return fullurl
}
func (d DSN) envelopeEndpoint() string {
	var fullurl string
	if d.host == "ingest.sentry.io" {
		fullurl = fmt.Sprint("https://", d.host, "/api/", d.projectId, "/envelope/?sentry_key=", d.key, "&sentry_version=7")
	}
	if d.host == "localhost:9000" {
		fullurl = fmt.Sprint("http://", d.host, "/api/", d.projectId, "/envelope/?sentry_key=", d.key, "&sentry_version=7")
	}
	if fullurl == "" {
		log.Fatal("problem with fullurl")
	}
	return fullurl
}

type Event struct {
	Platform    string `json:"platform"`
	Kind        string `json:"kind"`
	Headers     map[string]string `json:"headers"`
	Body        string `json:"body"`
}

func (e Event) String() string {
	return fmt.Sprintf("\n Event { Platform: %s, Type: %s }\n", e.Platform, e.Kind) // index somehow?
}

func jsEncoder(body map[string]interface{}) []byte {
	return marshalJSON(body)
}
func pyEncoder(body map[string]interface{}) []byte {
	bodyBytes := marshalJSON(body)
	buf := encodeGzip(bodyBytes)
	return buf.Bytes()
}

type BodyEncoder func(map[string]interface{}) []byte
type Timestamper func(map[string]interface{}, string) map[string]interface{}

func matchDSN(projectDSNs map[string]*DSN, event Event) string {
	
	// for getsentry/tracing-example (a situation where you have 3 Python Projects)
	// headers := event.Headers
	// if headers["X-Sentry-Auth"] != "" {
	// 	xSentryAuth := headers["X-Sentry-Auth"]
	// 	for _, projectDSN := range projectDSNs {
	// 		if strings.Contains(xSentryAuth, projectDSN.key) {
	// 			fmt.Println("> match", projectDSN)
	// 			return projectDSN.storeEndpoint()
	// 		}
	// 	}
	// }

	platform := event.Platform

	var storeEndpoint string
	if platform == "javascript" && event.Kind == "error" {
		storeEndpoint = projectDSNs["javascript"].storeEndpoint()
	} else if platform == "python" && event.Kind == "error" {
		storeEndpoint = projectDSNs["python"].storeEndpoint()
	} else if platform == "android" && event.Kind == "error" {
		storeEndpoint = projectDSNs["android"].storeEndpoint()
	} else if platform == "javascript" && event.Kind == "transaction" {
		storeEndpoint = projectDSNs["javascript"].envelopeEndpoint()
	} else if platform == "python" && event.Kind == "transaction" {
		storeEndpoint = projectDSNs["python"].envelopeEndpoint()
	} else if platform == "android" && event.Kind == "transaction" {
		storeEndpoint = projectDSNs["android"].envelopeEndpoint()
	} else {
		log.Fatal("platform type not supported")
	}

	if storeEndpoint == "" {
		log.Fatal("missing store endpoint")
	}
	return storeEndpoint
}

func init() {
	if err := godotenv.Load(); err != nil {
		log.Print("No .env file found")
	}

	all = flag.Bool("all", false, "send all events. default is send latest event")
	id = flag.String("id", "", "id of event in sqlite database") // 08/27 non-functional today
	ignore = flag.Bool("i", false, "ignore sending the event to Sentry.io")
	db = flag.String("db", "", "database.json")
	js = flag.String("js", "", "javascript DSN")
	py = flag.String("py", "", "python DSN")
	flag.Parse()

	// Use SAAS DSN's for Tx's as getsentry/sentry 10.0.0 doesn't support Tx's yet
	projectDSNs = make(map[string]*DSN)
	projectDSNs["javascript"] = parseDSN(os.Getenv("DSN_JAVASCRIPT_SAAS"))
	if (*js != "") {
		projectDSNs["javascript"] = parseDSN(*js)
	}
	projectDSNs["python"] = parseDSN(os.Getenv("DSN_PYTHON_SAAS"))
	if (*py != "") {
		projectDSNs["python"] = parseDSN(*py)
	}

	// TODO "panic: runtime error: slice bounds out of range [7:0]" if these are not set
	// projectDSNs["android"] = parseDSN(os.Getenv("DSN_ANDROID"))
	// projectDSNs["python_gateway"] = parseDSN(os.Getenv("DSN_PYTHON_GATEWAY"))
	// projectDSNs["python_django"] = parseDSN(os.Getenv("DSN_PYTHON_DJANGO"))
	// projectDSNs["python_celery"] = parseDSN(os.Getenv("DSN_PYTHON_CELERY"))

	// fmt.Println("> --db json flag", *db)
	if *db == "" {
		database = os.Getenv("JSON")
	} else {
		database = *db
	}
}

func main() {
	jsonFile, err := os.Open(database)
	if err != nil {
		log.Fatal(err)
	}

	byteValue, _ := ioutil.ReadAll(jsonFile)
	defer jsonFile.Close()
	events := make([]Event, 0)
	if err := json.Unmarshal(byteValue, &events); err != nil {
		panic(err)
	}

	// TODO rename body as errorBody or eventPayload?
	for idx, event := range events {
		fmt.Printf("> EVENT# %v \n", idx)

		var body map[string]interface{}
		var envelope string
		var timestamper Timestamper 
		var bodyEncoder BodyEncoder
		var headerKeys []string
		fmt.Println(headerKeys)
		var storeEndpoint string
		var requestBody []byte

		if (event.Kind == "error") {			
			body, timestamper, bodyEncoder, headerKeys, storeEndpoint = decodeEvent(event)
			body = eventId(body)
			body = release(body)
			body = user(body)
			body = timestamper(body, event.Platform)
			undertake(body)
			requestBody = bodyEncoder(body)
		} else if (event.Kind == "transaction") {
			
			envelope, timestamper, bodyEncoder, headerKeys, storeEndpoint = decodeEnvelope(event)

			fmt.Printf(" %T %T %T %T\n", timestamper, bodyEncoder, headerKeys, storeEndpoint)

			// TODO transform the envelope Array, update traceId, release, user, timestamps
			// TODO bodyEncoder() it again
			
			requestBody = []byte(envelope)
		}

		request := buildRequest(requestBody, event.Headers, storeEndpoint)

		if !*ignore {
			response, requestErr := httpClient.Do(request)
			if requestErr != nil {
				log.Fatal(requestErr)
			}

			responseData, responseDataErr := ioutil.ReadAll(response.Body)
			if responseDataErr != nil {
				log.Fatal(responseDataErr)
			}

			fmt.Printf("\n> EVENT KIND: %s | RESPONSE: %s\n", event.Kind, string(responseData))
		} else {
			fmt.Printf("\n> %s event IGNORED", event.Kind)
		}

		// TODO - break early, or auto-select 1 before the for loop
		// if !*all {
		// 	return
		// }

		time.Sleep(1000 * time.Millisecond)
	}
	return
}

// TODO remove 'TRANSACTION' from here
func decodeEvent(event Event) (map[string]interface{}, Timestamper, BodyEncoder, []string, string) {

	body := unmarshalJSON([]byte(event.Body))

	JAVASCRIPT := event.Platform == "javascript"
	PYTHON := event.Platform == "python"
	ANDROID := event.Platform == "android"

	ERROR := event.Kind == "error"
	TRANSACTION := event.Kind == "transaction"

	jsHeaders := []string{"Accept-Encoding", "Content-Length", "Content-Type", "User-Agent"}
	pyHeaders := []string{"Accept-Encoding", "Content-Length", "Content-Encoding", "Content-Type", "User-Agent"}
	androidHeaders := []string{"Content-Length","User-Agent","Connection","Content-Encoding","X-Forwarded-Proto","Host","Accept","X-Forwarded-For"} // X-Sentry-Auth omitted
	storeEndpoint := matchDSN(projectDSNs, event)
	fmt.Printf("> storeEndpoint %v \n", storeEndpoint)

	switch {
	case ANDROID && TRANSACTION:
		return body, updateTimestamp, pyEncoder, androidHeaders, storeEndpoint
	case ANDROID && ERROR:
		return body, updateTimestamp, pyEncoder, androidHeaders, storeEndpoint

	case JAVASCRIPT && TRANSACTION:
		return body, updateTimestamps, jsEncoder, jsHeaders, storeEndpoint
	case JAVASCRIPT && ERROR:
		return body, updateTimestamp, jsEncoder, jsHeaders, storeEndpoint

	case PYTHON && TRANSACTION:
		return body, updateTimestamps, pyEncoder, pyHeaders, storeEndpoint
	case PYTHON && ERROR:
		return body, updateTimestamp, pyEncoder, pyHeaders, storeEndpoint
	}

	return body, updateTimestamps, jsEncoder, jsHeaders, storeEndpoint
}

func buildRequest(requestBody []byte, eventHeaders map[string]string, storeEndpoint string) *http.Request {
	request, errNewRequest := http.NewRequest("POST", storeEndpoint, bytes.NewReader(requestBody)) // &buf
	if errNewRequest != nil {
		log.Fatalln(errNewRequest)
	}

	for key, value := range eventHeaders {
		request.Header.Set(key, value)
	}
	return request
}

// same eventId cannot be accepted twice by Sentry
func eventId(body map[string]interface{}) map[string]interface{} {
	if _, ok := body["event_id"]; !ok {
		log.Print("no event_id on object from DB")
	}
	var uuid4 = strings.ReplaceAll(uuid.New().String(), "-", "")
	body["event_id"] = uuid4
	fmt.Println("> event_id updated", body["event_id"])
	return body
}

// CalVer-lite
func release(body map[string]interface{}) map[string]interface{} {
	date := time.Now()
	month := date.Month()
	day := date.Day()
	var week int
	switch {
	case day <= 7:
		week = 1
	case day >= 8 && day <= 14:
		week = 2
	case day >= 15 && day <= 21:
		week = 3
	case day >= 22:
		week = 4
	}
	release := fmt.Sprint(int(month), ".", week)
	body["release"] = release
	fmt.Println("> release", body["release"])
	return body
}

func user(body map[string]interface{}) map[string]interface{} {
	if body["user"] == nil {
		body["user"] = make(map[string]interface{})
		user := body["user"].(map[string]interface{})
		rand.Seed(time.Now().UnixNano())
		alpha := strings.Split("abcdefghijklmnopqrstuvwxyz", "")[rand.Intn(9)]
		var alphanumeric string
		for i := 0; i < 3; i++ {
			alphanumeric += strings.Split("abcdefghijklmnopqrstuvwxyz0123456789", "")[rand.Intn(35)]
		}
		user["email"] = fmt.Sprint(alpha, alphanumeric, "@yahoo.com")
	}
	return body
}

func undertake(body map[string]interface{}) {
	if body["tags"] == nil {
		body["tags"] = make(map[string]interface{})
	}
	tags := body["tags"].(map[string]interface{})
	tags["undertaker"] = "h4ckweek"
}
