package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"grafana-usageeventbackend-datasource/pkg/models"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	_ "github.com/lib/pq"
)

// --- INTERFACE COMPLIANCE ---
var (
	_ backend.QueryDataHandler      = (*Datasource)(nil)
	_ backend.CheckHealthHandler    = (*Datasource)(nil)
	_ backend.CallResourceHandler   = (*Datasource)(nil)
	_ instancemgmt.InstanceDisposer = (*Datasource)(nil)
)

// --- TYPES ---
type Datasource struct {
	db         *sql.DB
	grafanaURL string
	apiKey     string
}

type UsageEventRequest struct {
	DashboardUID string  `json:"dashboard_uid"`
	Username     string  `json:"username"`
	UserID       *string `json:"user_id"`
	Timestamp    string  `json:"timestamp"` // ISO-8601 string
}

// Dashboard API response struct (partial)
type dashboardAPIResp struct {
	Dashboard struct {
		ID    int64  `json:"id"`
		UID   string `json:"uid"`
		Title string `json:"title"`
	} `json:"dashboard"`
	Meta struct {
		URL string `json:"url"`
	} `json:"meta"`
}

// --- INSTANCE/DB INIT ---
func NewDatasource(_ context.Context, dsSettings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	config, err := models.LoadPluginSettings(dsSettings)
	if err != nil {
		backend.Logger.Error("Failed to load plugin settings", "err", err)
		return nil, err
	}

	dbHost := os.Getenv("GF_DATABASE_HOST")
	dbPort := os.Getenv("GF_DATABASE_PORT")
	dbUser := os.Getenv("GF_DATABASE_USER")
	dbPass := os.Getenv("GF_DATABASE_PASSWORD")
	dbName := os.Getenv("GF_DATABASE_NAME")
	sslMode := os.Getenv("GF_DATABASE_SSL_MODE")

	if dbHost == "" {
		dbHost = "localhost"
	}
	if dbPort == "" {
		dbPort = "5432"
	}
	if dbUser == "" {
		dbUser = "postgres"
	}
	if dbPass == "" {
		dbPass = "admin"
	}
	if dbName == "" {
		dbName = "grafana"
	}
	if sslMode == "" {
		sslMode = "disable"
	}

	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		dbHost, dbPort, dbUser, dbPass, dbName, sslMode)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		backend.Logger.Error("Failed to open db", "err", err)
		return nil, fmt.Errorf("failed to open db: %w", err)
	}

	// Tune connection pool
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

	if err := db.Ping(); err != nil {
		backend.Logger.Error("Failed to connect db", "err", err)
		return nil, fmt.Errorf("failed to connect db: %w", err)
	}

	// Use API key from secure json settings
	apiKey := ""
	if v, ok := dsSettings.DecryptedSecureJSONData["apiKey"]; ok {
		apiKey = v
	} else {
		backend.Logger.Warn("API key for Grafana not set in secure settings")
	}

	if config.Path != "" {
		// build the URL from path or use path as base URL if it is full URL
		config.GrafanaURL = config.Path
	}

	backend.Logger.Info("Datasource initialized successfully")
	backend.Logger.Info("Using Grafana URL", "url", config.GrafanaURL)

	return &Datasource{
		db:         db,
		grafanaURL: config.GrafanaURL,
		apiKey:     apiKey,
	}, nil
}

func (d *Datasource) Dispose() {
	if d.db != nil {
		d.db.Close()
	}
}

// --- RESOURCE HANDLER ---
func (d *Datasource) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) (err error) {
	defer func() {
		if r := recover(); r != nil {
			backend.Logger.Error("[PANIC RECOVER]", "panic", r)
			err = sender.Send(&backend.CallResourceResponse{
				Status: http.StatusInternalServerError,
				Body:   []byte(fmt.Sprintf("internal server error: %v", r)),
			})
		}
	}()

	backend.Logger.Info("CallResource called", "path", req.Path, "method", req.Method)

	switch req.Path {
	case "usage-event":
		if req.Method != http.MethodPost {
			backend.Logger.Warn("Method not allowed", "method", req.Method)
			return sender.Send(&backend.CallResourceResponse{
				Status: http.StatusMethodNotAllowed,
				Body:   []byte("method not allowed"),
			})
		}

		var evt UsageEventRequest
		if err := json.Unmarshal(req.Body, &evt); err != nil {
			backend.Logger.Error("Invalid JSON", "err", err)
			return sender.Send(&backend.CallResourceResponse{
				Status: http.StatusBadRequest,
				Body:   []byte("invalid JSON"),
			})
		}
		backend.Logger.Info("Received usage event", "event", evt)

		// Parse timestamp
		eventTime, err := time.Parse(time.RFC3339, evt.Timestamp)
		if err != nil {
			backend.Logger.Warn("Invalid timestamp format, using now", "err", err)
			eventTime = time.Now()
		}

		// Fetch dashboard details with fallback
		dashID, dashTitle, dashURL, err := d.fetchDashboardDetails(evt.DashboardUID)
		if err != nil {
			backend.Logger.Warn("Failed to fetch dashboard details, will fallback to limited info", "err", err)
			dashID = 0
			dashTitle = ""
			dashURL = ""
		}

		// Determine application_name based on dashboard title
		applicationName := ""
		titleLower := strings.ToLower(dashTitle)
		switch {
		case strings.Contains(titleLower, "ava"):
			applicationName = "AVA"
		case strings.Contains(titleLower, "clinical"):
			applicationName = "Clinical"
		case strings.Contains(titleLower, "docfind"):
			applicationName = "DocFind"
		case strings.Contains(titleLower, "documentinquiryv1"):
			applicationName = "DocumentInquiryv1"
		case strings.Contains(titleLower, "documentinquiryvII"):
			applicationName = "DocumentInquiryvII"
		case strings.Contains(titleLower, "documentviewer"):
			applicationName = "documentviewer"
		case strings.Contains(titleLower, "edi"):
			applicationName = "EDI-Gateway"
		case strings.Contains(titleLower, "eicorrespondence"):
			applicationName = "EICorrespondence"
		case strings.Contains(titleLower, "esds"):
			applicationName = "ESDS"
		case strings.Contains(titleLower, "iop"):
			applicationName = "IOP"
		case strings.Contains(titleLower, "ipp"):
			applicationName = "IPP"
		case strings.Contains(titleLower, "transparency"):
			applicationName = "IVL_Transparency"
		case strings.Contains(titleLower, "kana"):
			applicationName = "KANA"
		case strings.Contains(titleLower, "medicaidwebportal"):
			applicationName = "MedicaidWebPortal"
		case strings.Contains(titleLower, "odm"):
			applicationName = "ODM"
		case strings.Contains(titleLower, "ppapi"):
			applicationName = "PPAPI"
		case strings.Contains(titleLower, "rxbor"):
			applicationName = "RxBoR"
		case strings.Contains(titleLower, "smart"):
			applicationName = "Smart"
		case strings.Contains(titleLower, "yava"):
			applicationName = "YAVA"
		case strings.Contains(titleLower, "usage"):
			applicationName = "UsageMetricsDashboard"
		case strings.Contains(titleLower, "cec"):
			applicationName = "CEC"
		case strings.Contains(titleLower, "node"):
			applicationName = "Node"
		default:
			applicationName = ""
		}

		// Determine signal value based on dashboard title
		signal := ""
		titleLowervar2 := strings.ToLower(dashTitle)
		switch {
		case strings.Contains(titleLowervar2, "log"):
			signal = "Logs"
		case strings.Contains(titleLowervar2, "trace") || strings.Contains(titleLowervar2, "tracing"):
			signal = "Traces"
		default:
			signal = "Metrics"
		}

		userID := evt.UserID

		// Insert into DB - always attempt insert, even if dashboard fetch failed
		_, err = d.db.Exec(`
			INSERT INTO usage_event (
				dashboard_id, dashboard_uid, dashboard_title, dashboard_url,
				user_id, username, application_name, event_time, signal
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`,
			dashID, evt.DashboardUID, dashTitle, dashURL,
			userID, evt.Username, applicationName, eventTime, signal,
		)
		if err != nil {
			errMsg := fmt.Sprintf("db error: %v", err)
			backend.Logger.Error(errMsg)
			return sender.Send(&backend.CallResourceResponse{
				Status: http.StatusInternalServerError,
				Body:   []byte(errMsg),
			})
		}

		backend.Logger.Info("Usage event inserted successfully")
		return sender.Send(&backend.CallResourceResponse{Status: http.StatusNoContent})

	default:
		backend.Logger.Error("Not found resource path", "path", req.Path)
		return sender.Send(&backend.CallResourceResponse{
			Status: http.StatusNotFound,
			Body:   []byte("not found"),
		})
	}
}

// --- DASHBOARD DETAILS FETCH ---
func (d *Datasource) fetchDashboardDetails(uid string) (int64, string, string, error) {
	url := fmt.Sprintf("%s/api/dashboards/uid/%s", d.grafanaURL, uid)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+d.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return 0, "", "", fmt.Errorf("dashboard API status %d: %s", resp.StatusCode, string(body))
	}

	var out dashboardAPIResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, "", "", err
	}
	return out.Dashboard.ID, out.Dashboard.Title, out.Meta.URL, nil
}

// --- QUERY/DUMMY/HEALTH HANDLERS ---
func (d *Datasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	response := backend.NewQueryDataResponse()
	for _, q := range req.Queries {
		response.Responses[q.RefID] = backend.DataResponse{}
	}
	return response, nil
}

func (d *Datasource) CheckHealth(_ context.Context, req *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	res := &backend.CheckHealthResult{
		Status:  backend.HealthStatusOk,
		Message: "Data source is working",
	}
	return res, nil
}
