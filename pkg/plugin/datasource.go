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
	_ "github.com/lib/pq" // PostgreSQL driver
)

var (
	_ backend.QueryDataHandler      = (*Datasource)(nil)
	_ backend.CheckHealthHandler    = (*Datasource)(nil)
	_ backend.CallResourceHandler   = (*Datasource)(nil)
	_ instancemgmt.InstanceDisposer = (*Datasource)(nil)
)

type Datasource struct {
	db         *sql.DB
	grafanaURL string
	apiKey     string
}

// --- STRUCTS ---
type UsageEventRequest struct {
	DashboardUID string  `json:"dashboard_uid"`
	Username     string  `json:"username"`
	UserID       *string `json:"user_id,omitempty"`
	DashboardURL string  `json:"dashboard_url,omitempty"`
}

type dashboardAPIResp struct {
	Dashboard struct {
		ID    int64  `json:"id"`
		UID   string `json:"uid"`
		Title string `json:"title"`
	} `json:"dashboard"`
}

// ------- INSTANCE & DB INIT -------
func NewDatasource(_ context.Context, dsSettings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	config, err := models.LoadPluginSettings(dsSettings)
	if err != nil {
		backend.Logger.Error("Failed to load plugin settings", "err", err)
		return nil, err
	}

	// Trim API key to avoid whitespace issues
	apiKey := strings.TrimSpace(config.Secrets.ApiKey)

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
	if err := db.Ping(); err != nil {
		backend.Logger.Error("Failed to connect db", "err", err)
		return nil, fmt.Errorf("failed to connect db: %w", err)
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

// ------- RESOURCE HANDLER -------
func (d *Datasource) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) (err error) {
	defer func() {
		if r := recover(); r != nil {
			backend.Logger.Error("[PANIC RECOVER]", "panic", r)
			err = sender.Send(&backend.CallResourceResponse{
				Status: http.StatusInternalServerError,
				Body:   []byte(fmt.Sprintf("Internal server error: %v", r)),
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
				Body:   []byte("Method not allowed"),
			})
		}

		var evt UsageEventRequest
		if err := json.Unmarshal(req.Body, &evt); err != nil {
			backend.Logger.Error("Invalid JSON", "err", err)
			return sender.Send(&backend.CallResourceResponse{
				Status: http.StatusBadRequest,
				Body:   []byte("Invalid JSON"),
			})
		}
		backend.Logger.Debug("Received usage event", "event", evt)

		dashID, dashTitle, err := d.fetchDashboardDetails(evt.DashboardUID)
		if err != nil {
			errMsg := fmt.Sprintf("Failed to fetch dashboard: %v", err)
			backend.Logger.Error(errMsg)
			return sender.Send(&backend.CallResourceResponse{
				Status: http.StatusInternalServerError,
				Body:   []byte(errMsg),
			})
		}

		now := time.Now()

		// Log final POST request data before DB insert
		backend.Logger.Info("Inserting usage event",
			"dashboard_id", dashID,
			"dashboard_uid", evt.DashboardUID,
			"dashboard_title", dashTitle,
			"dashboard_url", evt.DashboardURL,
			"user_id", evt.UserID,
			"username", evt.Username,
			"event_time", now.String(),
		)

		_, err = d.db.Exec(`
            INSERT INTO usage_event (
                dashboard_id, dashboard_uid, dashboard_title, dashboard_url, user_id, username, event_time
            ) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			dashID, evt.DashboardUID, dashTitle, evt.DashboardURL, evt.UserID, evt.Username, now)

		if err != nil {
			errMsg := fmt.Sprintf("DB error: %v", err)
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
			Body:   []byte("Not found"),
		})
	}
}

// ------ UTIL: Fetch Dash Metadata ------
func (d *Datasource) fetchDashboardDetails(uid string) (int64, string, error) {
	url := fmt.Sprintf("%s/api/dashboards/uid/%s", d.grafanaURL, uid)
	backend.Logger.Info("Fetching dashboard details", "url", url)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		backend.Logger.Error("Failed building dashboard fetch request", "err", err)
		return 0, "", err
	}

	// apiKey := strings.TrimSpace(d.apiKey)
	backend.Logger.Debug("Setting Authorization header")
	req.Header.Set("Authorization", "Bearer "+"")

	// Debug log all headers before sending
	for k, v := range req.Header {
		backend.Logger.Info("Request header", "key", k, "value", strings.Join(v, ","))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		backend.Logger.Error("Dashboard fetch HTTP call failed", "err", err)
		return 0, "", err
	}
	defer resp.Body.Close()

	backend.Logger.Info("Dashboard fetch response status", "status", resp.StatusCode)
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		backend.Logger.Error("Dashboard API call non-200", "status", resp.StatusCode, "body", string(body))
		return 0, "", fmt.Errorf("API status %d: %s", resp.StatusCode, string(body))
	}

	var out dashboardAPIResp
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&out); err != nil {
		backend.Logger.Error("Failed to decode dashboard API response", "err", err)
		return 0, "", err
	}

	backend.Logger.Info("Dashboard fetched successfully", "dashboard_id", out.Dashboard.ID, "title", out.Dashboard.Title)
	return out.Dashboard.ID, out.Dashboard.Title, nil
}

// -------- TEMPLATE: Query/Health --------
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
