package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/gorilla/websocket"
	"github.com/keptn/keptn/cli/utils/websockethelper"
	"github.com/keptn/keptn/configuration-service/models"
	websocketutil "github.com/keptn/keptn/shipyard-service/websocketutil"

	"github.com/cloudevents/sdk-go/pkg/cloudevents"
	"github.com/cloudevents/sdk-go/pkg/cloudevents/client"
	cloudeventshttp "github.com/cloudevents/sdk-go/pkg/cloudevents/transport/http"
	"github.com/cloudevents/sdk-go/pkg/cloudevents/types"
	"github.com/google/uuid"
	"github.com/kelseyhightower/envconfig"
	keptnutils "github.com/keptn/go-utils/pkg/utils"
)

const timeout = 60
const configservice = "CONFIGURATION_SERVICE"
const eventbroker = "EVENTBROKER"
const api = "API"

type envConfig struct {
	Port int    `envconfig:"RCV_PORT" default:"8080"`
	Path string `envconfig:"RCV_PATH" default:"/"`
}

type createProjectEventData struct {
	Project      string          `json:"project"`
	GitRemoteURL string          `json:"gitremoteurl"`
	GitUser      string          `json:"gituser"`
	GitToken     string          `json:"gittoken"`
	Shipyard     []shipyardStage `json:"stages"`
}

type shipyardStage struct {
	Name               string `json:"name"`
	DeplyomentStrategy string `json:"deployment_strategy"`
	TestStrategy       string `json:"test_strategy"`
}

type doneEventData struct {
	Result  string `json:"result"`
	Message string `json:"message"`
	Version string `json:"version"`
}

type Client struct {
	httpClient *http.Client
}

func main() {
	var env envConfig
	if err := envconfig.Process("", &env); err != nil {
		log.Fatalf("failed to process env var: %s", err)
	}
	os.Exit(_main(os.Args[1:], env))
}

func _main(args []string, env envConfig) int {

	ctx := context.Background()

	t, err := cloudeventshttp.New(
		cloudeventshttp.WithPort(env.Port),
		cloudeventshttp.WithPath(env.Path),
	)
	if err != nil {
		log.Fatalf("failed to create transport: %v", err)
	}

	c, err := client.New(t)
	if err != nil {
		log.Fatalf("failed to create client: %v", err)
	}
	log.Fatalf("failed to start receiver: %s", c.StartReceiver(ctx, gotEvent))

	return 0
}

func newClient() *Client {
	client := Client{
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
	return &client
}

func gotEvent(ctx context.Context, event cloudevents.Event) error {
	var shkeptncontext string
	event.Context.ExtensionAs("shkeptncontext", &shkeptncontext)

	logger := keptnutils.NewLogger(shkeptncontext, event.Context.GetID(), "shipyard-service")

	// open websocket connection to api component
	endPoint, err := getServiceEndpoint(api)
	if err != nil {
		return err
	}

	if endPoint.Host == "" {
		const errorMsg = "Host of api not set"
		logger.Error(errorMsg)
		return errors.New(errorMsg)
	}

	connData := &websockethelper.ConnectionData{}
	if err := event.DataAs(connData); err != nil {
		logger.Error(fmt.Sprintf("Data of the event is incompatible: %s", err.Error()))
		return err
	}

	ws, _, err := websocketutil.OpenWS(*connData, endPoint)
	if err != nil {
		logger.Error(fmt.Sprintf("Opening websocket connection failed: %s", err.Error()))
		return err
	}
	defer ws.Close()

	//if event.Type() == "sh.keptn.internal.events.project.create" { // for keptn internal topics
	if event.Type() == "create.project" {
		version, err := createProjectAndProcessShipyard(event, *logger, ws)
		if err := respondWithDoneEvent(event, version, err, *logger, ws); err != nil {
			logger.Error(fmt.Sprintf("No sh.keptn.event.done sent: %s", err.Error()))
			return err
		}
		return nil
	}

	const errorMsg = "Received unexpected keptn event that cannot be processed"
	if err := websocketutil.WriteWSLog(ws, createEventCopy(event, "sh.keptn.events.log"), errorMsg, true, "INFO"); err != nil {
		logger.Error(fmt.Sprintf("Could not write log to websocket: %s", err.Error()))
	}
	logger.Error(errorMsg)
	return errors.New(errorMsg)
}

// createProjectAndProcessShipyard creates a project and stages defined in the shipyard
func createProjectAndProcessShipyard(event cloudevents.Event, logger keptnutils.Logger, ws *websocket.Conn) (*models.Version, error) {
	eventData := &createProjectEventData{}
	if err := event.DataAs(eventData); err != nil {
		logger.Error(fmt.Sprintf("Data of the event is incompatible: %s", err.Error()))
		return nil, err
	}

	client := newClient()
	// create project
	project := models.Project{}
	project.ProjectName = eventData.Project
	if err := client.createProject(project, logger); err != nil {
		logger.Error(err.Error())
		return nil, fmt.Errorf("Creating project %s failed: %s", project.ProjectName, err.Error())
	}
	if err := websocketutil.WriteWSLog(ws, createEventCopy(event, "sh.keptn.events.log"), fmt.Sprintf("%s created", project.ProjectName), false, "INFO"); err != nil {
		logger.Error(fmt.Sprintf("Could not write log to websocket: %s", err.Error()))
	}

	// process shipyard file and create stages
	for _, shipyardStage := range eventData.Shipyard {
		stage := models.Stage{}
		stage.StageName = shipyardStage.Name

		if err := client.createStage(project, stage, logger); err != nil {
			logger.Error(err.Error())
			return nil, fmt.Errorf("Creating stage %s failed: %s", stage.StageName, err.Error())
		}
		if err := websocketutil.WriteWSLog(ws, createEventCopy(event, "sh.keptn.events.log"), fmt.Sprintf("%s created", stage.StageName), false, "INFO"); err != nil {
			logger.Error(fmt.Sprintf("Could not write log to websocket: %s", err.Error()))
		}
	}

	// store shipyard.yaml
	shipyard := models.Resource{}

	var resourceURI = "shipyard.yaml"
	shipyard.ResourceURI = &resourceURI
	shipyard.ResourceContent, _ = json.Marshal(eventData.Shipyard)

	resources := []*models.Resource{&shipyard}
	version, err := client.storeResource(project, resources, logger)
	if err != nil {
		logger.Error(err.Error())
		return nil, errors.New("Storing shipyard.yaml file failed")
	}

	return version, nil
}

// respondWithDoneEvent sends a keptn done event to the keptn eventbroker
func respondWithDoneEvent(event cloudevents.Event, version *models.Version, err error, logger keptnutils.Logger, ws *websocket.Conn) error {
	if err != nil {
		// error
		logger.Error(err.Error())

		if err := websocketutil.WriteWSLog(ws, createEventCopy(event, "sh.keptn.events.log"), fmt.Sprintf("%s", err.Error()), true, "INFO"); err != nil {
			logger.Error(fmt.Sprintf("could not write log to websocket: %s", err.Error()))
		}
		if err := sendDoneEvent(event, "error", err.Error(), version); err != nil {
			logger.Error(err.Error())
		}

		return err

	}
	// success
	const successMsg = "Project created and shipyard successfully processed"
	logger.Info(successMsg)

	if err := websocketutil.WriteWSLog(ws, createEventCopy(event, "sh.keptn.events.log"), successMsg, true, "INFO"); err != nil {
		logger.Error(fmt.Sprintf("could not write log to websocket: %s", err.Error()))
	}
	if err := sendDoneEvent(event, "success", successMsg, version); err != nil {
		logger.Error(err.Error())
		return err
	}

	return nil
}

func (client *Client) createProject(project models.Project, logger keptnutils.Logger) error {
	data, err := project.MarshalBinary()
	if err != nil {
		logger.Error(err.Error())
		return errors.New("Failed to create project")
	}

	resp, err := postRequest(client, "/project", data)
	if err != nil {
		logger.Error(err.Error())
		return errors.New("Failed to create project")
	}

	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK { // 204 - Success. Project has been created. Response does not have a body.
		logger.Info("Project successfully created")
	} else if resp.StatusCode == http.StatusBadRequest { //	400 - Failed. Project could not be created.
		errorObj := models.Error{}
		json.NewDecoder(resp.Body).Decode(&errorObj)
		return errors.New(*errorObj.Message)
	} else { // catch undefined errors
		return errors.New("Undefined error in response of creating project")
	}

	return nil
}

func (client *Client) createStage(project models.Project, stage models.Stage, logger keptnutils.Logger) error {
	data, err := project.MarshalBinary()
	if err != nil {
		logger.Error(err.Error())
		return errors.New("Failed to create stage")
	}

	resp, err := postRequest(client, fmt.Sprintf("project/%s/stage", project.ProjectName), data)
	if err != nil {
		logger.Error(err.Error())
		return errors.New("Failed to create stage")
	}

	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent { // 204 - Success. Stage has been created. Response does not have a body.
		logger.Info("Stage successfully created")
	} else if resp.StatusCode == http.StatusBadRequest { //	400 - Failed. Stage could not be created.
		errorObj := models.Error{}
		json.NewDecoder(resp.Body).Decode(&errorObj)
		return errors.New(*errorObj.Message)
	} else { // catch undefined errors
		return errors.New("Undefined error in response of creating stage")
	}

	return nil
}

func (client *Client) storeResource(project models.Project, resources []*models.Resource, logger keptnutils.Logger) (*models.Version, error) {
	data, err := project.MarshalBinary()
	if err != nil {
		logger.Error(err.Error())
		return nil, errors.New("Failed to store resource")
	}

	resp, err := postRequest(client, fmt.Sprintf("project/%s/resource", project.ProjectName), data)
	if err != nil {
		logger.Error(err.Error())
		return nil, errors.New("Failed to store resource")
	}

	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated { // 201 - Success. Stage has been created. Response does not have a body.
		versionObj := models.Version{}
		json.NewDecoder(resp.Body).Decode(&versionObj)
		logger.Info("Resource successfully stored")

		return &versionObj, nil
	} else if resp.StatusCode == http.StatusBadRequest { //	400 - Failed. Stage could not be created.
		errorObj := models.Error{}
		json.NewDecoder(resp.Body).Decode(&errorObj)

		return nil, errors.New(*errorObj.Message)
	} else { // catch undefined errors
		return nil, errors.New("Undefined error in response of storing resource")
	}
}

// postRequest sends a post request
func postRequest(client *Client, path string, body []byte) (*http.Response, error) {
	endPoint, err := getServiceEndpoint(configservice)
	if err != nil {
		return nil, err
	}

	if endPoint.Host == "" {
		return nil, errors.New("Host of configuration-service not set")
	}

	eventURL := endPoint
	eventURL.Path = path

	req, err := http.NewRequest("POST", eventURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("Failed to build new request")
	}

	return client.httpClient.Do(req)
}

// getServiceEndpoint returns the endpoint of a service stored in an environment variable
func getServiceEndpoint(service string) (url.URL, error) {
	url, err := url.Parse(os.Getenv(service))
	if err != nil {
		return *url, fmt.Errorf("Failed to retrieve value from ENVIRONMENT_VARIABLE: %s", service)
	}
	return *url, nil
}

// createEventCopy creates a deep copy of a CloudEvent
func createEventCopy(eventSource cloudevents.Event, eventType string) cloudevents.Event {
	var shkeptncontext string
	eventSource.Context.ExtensionAs("shkeptncontext", &shkeptncontext)
	var shkeptnphaseid string
	eventSource.Context.ExtensionAs("shkeptnphaseid", &shkeptnphaseid)
	var shkeptnphase string
	eventSource.Context.ExtensionAs("shkeptnphase", &shkeptnphase)
	var shkeptnstepid string
	eventSource.Context.ExtensionAs("shkeptnstepid", &shkeptnstepid)
	var shkeptnstep string
	eventSource.Context.ExtensionAs("shkeptnstep", &shkeptnstep)

	source, _ := url.Parse("shipyard-service")
	contentType := "application/json"

	event := cloudevents.Event{
		Context: cloudevents.EventContextV02{
			ID:          uuid.New().String(),
			Type:        eventType,
			Source:      types.URLRef{URL: *source},
			ContentType: &contentType,
			Extensions: map[string]interface{}{
				"shkeptncontext": shkeptncontext,
				"shkeptnphaseid": shkeptnphaseid,
				"shkeptnphase":   shkeptnphase,
				"shkeptnstepid":  shkeptnstepid,
				"shkeptnstep":    shkeptnstep,
			},
		}.AsV02(),
	}

	return event
}

// sendDoneEvent prepares a keptn done event and sends it to the eventbroker
func sendDoneEvent(receivedEvent cloudevents.Event, result string, message string, version *models.Version) error {

	doneEvent := createEventCopy(receivedEvent, "sh.keptn.events.done")

	eventData := doneEventData{
		Result:  result,
		Message: message,
	}

	if version != nil {
		eventData.Version = version.Version
	}

	doneEvent.Data = eventData

	endPoint, err := getServiceEndpoint(eventbroker)
	if err != nil {
		return errors.New("Failed to retrieve endpoint of eventbroker: %s" + err.Error())
	}

	if endPoint.Host == "" {
		return errors.New("Host of eventbroker not set")
	}

	transport, err := cloudeventshttp.New(
		cloudeventshttp.WithTarget(endPoint.String()),
		cloudeventshttp.WithEncoding(cloudeventshttp.StructuredV02),
	)
	if err != nil {
		return errors.New("Failed to create transport: " + err.Error())
	}

	client, err := client.New(transport)
	if err != nil {
		return errors.New("Failed to create HTTP client: " + err.Error())
	}

	if _, err := client.Send(context.Background(), doneEvent); err != nil {
		return errors.New("Failed to send cloudevent sh.keptn.events.done: " + err.Error())
	}

	return nil
}
