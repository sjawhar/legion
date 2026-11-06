package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// cloudsqlCPUMetric is Cloud Monitoring's metric for a Cloud SQL instance's CPU: the fraction of
// the instance's vCPUs in use, a gauge Cloud SQL writes every 60 s. The database of the load-test
// deployment is a Cloud SQL instance, not a container, so this is where its CPU comes from.
const cloudsqlCPUMetric = "cloudsql.googleapis.com/database/cpu/utilization"

// cloudsqlMemoryMetric is Cloud Monitoring's metric for the memory the instance's database uses:
// "Total RAM usage in bytes. This metric reports the RAM usage of the database process, including
// the buffer/cache", a gauge written every 60 s like the CPU
// (https://cloud.google.com/monitoring/api/metrics_gcp_c, cloudsql). It is the one R6's "memory
// of … Postgres" means beside the containers' memory, which is each container's own working set,
// page cache included; database/memory/usage, the other candidate, leaves the buffer cache out
// and so reads far below what the instance is actually holding.
const cloudsqlMemoryMetric = "cloudsql.googleapis.com/database/memory/total_usage"

// cloudsqlMemoryQuotaMetric is the instance's RAM: "Maximum RAM size in bytes", the limit the
// memory above is a share of, as limit_cores is for the CPU. A window with no quota point is not
// a failure of the read: the memory is the reading R6 asks for, and the share of the quota is
// what the file can add when the instance reports one.
const cloudsqlMemoryQuotaMetric = "cloudsql.googleapis.com/database/memory/quota"

// cloudsqlAlignment is the alignment period the read asks for: 60 s, the period Cloud SQL writes
// this metric at, so one aligned point stands for one written sample and no point is invented.
const cloudsqlAlignment = 60 * time.Second

// cloudsqlMonitoringBase is Cloud Monitoring's v3 API.
const cloudsqlMonitoringBase = "https://monitoring.googleapis.com"

// cloudsqlTokenURL is the metadata server's access token for this pod's Google identity, which it
// holds through workload identity. The read carries the Metadata-Flavor: Google header the
// metadata server requires.
const cloudsqlTokenURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"

// cloudsqlDefaultVCPUs is the vCPU count a utilization fraction is turned into cores with:
// production's tier is db-custom-16-61440, 16 vCPUs.
const cloudsqlDefaultVCPUs = 16.0

// cloudsqlPageMax is how many pages of time series one window may take; a 60 s alignment over a
// step's window is at most a few hundred points, so a longer walk is a paging loop, not a window.
const cloudsqlPageMax = 32

// cloudsqlCPUFile is a step's cloudsql-cpu.json: the Cloud SQL instance's CPU and memory over the
// step's window, from Cloud Monitoring. The instance is not a container, so the file carries the
// two figures the pass rule judges a container by (cores_mean against limit_cores, as
// cgroup.json's roles do) beside the fraction Cloud Monitoring reports, and every sample behind
// them; R6 asks for Postgres's memory beside its CPU, and memory_bytes_mean is that reading.
type cloudsqlCPUFile struct {
	Project     string    `json:"project"`
	Instance    string    `json:"instance"`
	DatabaseID  string    `json:"database_id"`
	Metric      string    `json:"metric"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	WindowS     float64   `json:"window_s"`
	// AlignmentPeriodS is the alignment the read asked for, so a reader knows what one point is.
	AlignmentPeriodS float64 `json:"alignment_period_s"`
	Samples          int     `json:"samples"`
	// UtilizationMean and UtilizationMax are the metric itself: the share of the instance's vCPUs
	// in use, over the window's aligned points.
	UtilizationMean float64 `json:"utilization_mean"`
	UtilizationMax  float64 `json:"utilization_max"`
	// LimitCores is -vcpus, the instance's vCPU count the fraction is of; CoresMean and
	// CoresPeak60s are the fractions as cores, so criterion (d)'s rule (mean cores under 0.9 of
	// the limit) reads here as it reads on a container's cores_mean and limit_cores.
	LimitCores   float64            `json:"limit_cores"`
	CoresMean    float64            `json:"cores_mean"`
	CoresPeak60s float64            `json:"cores_peak_60s"`
	Points       []cloudsqlCPUPoint `json:"points"`
	// The memory of the same instance over the same window: which series it is, how many points
	// it had, and the mean and peak of them. A window Cloud Monitoring holds no memory point for
	// is refused, never written as 0 bytes.
	MemoryMetric    string  `json:"memory_metric"`
	MemorySamples   int     `json:"memory_samples"`
	MemoryBytesMean float64 `json:"memory_bytes_mean"`
	MemoryBytesMax  float64 `json:"memory_bytes_max"`
	// MemoryLimitBytes is the instance's RAM, from the quota series, and
	// MemoryUtilizationMean the mean memory as a share of it. Both are null on a window the
	// quota series holds no point for: the memory above is the reading, and its share of a
	// quota nobody reported is not 0.
	MemoryLimitBytes      *float64              `json:"memory_limit_bytes"`
	MemoryUtilizationMean *float64              `json:"memory_utilization_mean"`
	MemoryPoints          []cloudsqlMemoryPoint `json:"memory_points"`
}

// cloudsqlCPUPoint is one aligned sample: the end of its alignment period, the fraction, and the
// cores that fraction is of the stated vCPU count.
type cloudsqlCPUPoint struct {
	At          time.Time `json:"at"`
	Utilization float64   `json:"utilization"`
	Cores       float64   `json:"cores"`
}

// cloudsqlMemoryPoint is one aligned memory sample.
type cloudsqlMemoryPoint struct {
	At    time.Time `json:"at"`
	Bytes float64   `json:"bytes"`
}

// cloudsqlPoint is one aligned point of any of these series, as the read gives it: the end of its
// alignment period and its value, whichever of the API's value fields carried it.
type cloudsqlPoint struct {
	At    time.Time
	Value float64
}

// cloudsqlTimeSeriesAnswer is the part of projects.timeSeries.list's answer this read needs. A
// DOUBLE series (the CPU's fraction) carries doubleValue and an INT64 series (the memory's bytes)
// int64Value, which protobuf's JSON writes as a string, so both are read.
type cloudsqlTimeSeriesAnswer struct {
	TimeSeries []struct {
		Points []struct {
			Interval struct {
				StartTime time.Time `json:"startTime"`
				EndTime   time.Time `json:"endTime"`
			} `json:"interval"`
			Value cloudsqlValue `json:"value"`
		} `json:"points"`
	} `json:"timeSeries"`
	NextPageToken string `json:"nextPageToken"`
}

// cloudsqlValue is one point's value in either of the two forms these series use.
type cloudsqlValue struct {
	DoubleValue *float64 `json:"doubleValue"`
	Int64Value  *string  `json:"int64Value"`
}

// number is the value as a float, and false when the point carries neither form or an int64Value
// that is not a number.
func (v cloudsqlValue) number() (float64, bool) {
	if v.DoubleValue != nil {
		return *v.DoubleValue, true
	}
	if v.Int64Value != nil {
		n, err := strconv.ParseFloat(*v.Int64Value, 64)
		return n, err == nil
	}
	return 0, false
}

// cloudsqlDatabaseID is the database_id label of a Cloud SQL instance's monitored resource,
// <project>:<instance>.
func cloudsqlDatabaseID(project, instance string) string { return project + ":" + instance }

// cloudsqlFilter selects one instance's series of one metric.
func cloudsqlFilter(metric, databaseID string) string {
	return fmt.Sprintf("metric.type=%q AND resource.labels.database_id=%q", metric, databaseID)
}

// cloudsqlMetadataToken is the access token of this pod's Google identity, from the metadata
// server at tokenURL. Every failure is named: the metadata server is how a pod with no gcloud
// authenticates, so a read that cannot say who it is must not look like an instance with no CPU.
func cloudsqlMetadataToken(ctx context.Context, client *http.Client, tokenURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("the metadata server at %s gave no access token: %w", redactURL(tokenURL), err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", fmt.Errorf("the metadata server at %s gave no access token: %w", redactURL(tokenURL), err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the metadata server at %s answered %d %s, so this pod has no access token", redactURL(tokenURL), resp.StatusCode, trunc(b))
	}
	var answer struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(b, &answer); err != nil {
		return "", fmt.Errorf("the metadata server's answer at %s did not decode as JSON: %w", redactURL(tokenURL), err)
	}
	if answer.AccessToken == "" {
		return "", fmt.Errorf("the metadata server's answer at %s carries no access_token", redactURL(tokenURL))
	}
	return answer.AccessToken, nil
}

// readCloudSQLSeries is the instance's aligned points of one metric from ws to we, oldest first.
// It calls projects.timeSeries.list, the API's GET of a project's time series with the filter,
// the interval and the aggregation as query parameters, following its pages.
//
// The read is refused by name, never written as a zero: a 403 (an identity that may not read the
// project's time series), a 404 (no such project or route), any other status, an answer that does
// not decode, a filter that matched more than one instance, and a point with no value. A window
// with no point at all is the empty slice, which each caller judges: a window with no CPU and no
// memory is a failed reading (errNoCloudSQLSample), and one with no quota point is a file
// without the share of the quota.
func readCloudSQLSeries(ctx context.Context, client *http.Client, base, token, project, metric, databaseID string, ws, we time.Time) ([]cloudsqlPoint, error) {
	endpoint := strings.TrimSuffix(base, "/") + "/v3/projects/" + url.PathEscape(project) + "/timeSeries"
	var points []cloudsqlPoint
	pageToken := ""
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("filter", cloudsqlFilter(metric, databaseID))
		q.Set("interval.startTime", ws.UTC().Format(time.RFC3339))
		q.Set("interval.endTime", we.UTC().Format(time.RFC3339))
		q.Set("aggregation.alignmentPeriod", fmt.Sprintf("%ds", int(cloudsqlAlignment.Seconds())))
		q.Set("aggregation.perSeriesAligner", "ALIGN_MEAN")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("the read of %s for %s failed: %w", metric, databaseID, err)
		}
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("the read of %s for %s failed: %w", metric, databaseID, readErr)
		}
		switch resp.StatusCode {
		case http.StatusOK:
		case http.StatusForbidden:
			return nil, fmt.Errorf("Cloud Monitoring answered 403 to the read of %s for %s: this pod's Google identity may not read project %s's time series: %s",
				metric, databaseID, project, trunc(b))
		case http.StatusNotFound:
			return nil, fmt.Errorf("Cloud Monitoring answered 404 to the read of %s for %s: project %s or the timeSeries route is not there: %s",
				metric, databaseID, project, trunc(b))
		default:
			return nil, fmt.Errorf("Cloud Monitoring answered %d to the read of %s for %s: %s",
				resp.StatusCode, metric, databaseID, trunc(b))
		}
		var answer cloudsqlTimeSeriesAnswer
		if err := json.Unmarshal(b, &answer); err != nil {
			return nil, fmt.Errorf("Cloud Monitoring's answer for %s did not decode as a timeSeries list: %w: %s", databaseID, err, trunc(b))
		}
		if len(answer.TimeSeries) > 1 {
			return nil, fmt.Errorf("the filter for %s matched %d series of %s, not one; name one instance", databaseID, len(answer.TimeSeries), metric)
		}
		for _, series := range answer.TimeSeries {
			for i, p := range series.Points {
				v, ok := p.Value.number()
				if !ok {
					return nil, fmt.Errorf("point %d of Cloud Monitoring's %s series for %s carries no doubleValue and no int64Value", i, metric, databaseID)
				}
				points = append(points, cloudsqlPoint{At: p.Interval.EndTime, Value: v})
			}
		}
		if answer.NextPageToken == "" {
			break
		}
		if page == cloudsqlPageMax {
			return nil, fmt.Errorf("Cloud Monitoring is still paging the window of %s after %d pages", databaseID, cloudsqlPageMax)
		}
		pageToken = answer.NextPageToken
	}
	slices.SortFunc(points, func(a, b cloudsqlPoint) int { return a.At.Compare(b.At) })
	return points, nil
}

// errNoCloudSQLSample refuses a window Cloud Monitoring holds no point of a reading for. The
// step's file must not carry a 0 for it: an instance nobody measured is not an idle instance.
func errNoCloudSQLSample(metric, databaseID string, ws, we time.Time) error {
	return fmt.Errorf("Cloud Monitoring holds no sample of %s for %s from %s to %s: a window with no sample is not 0",
		metric, databaseID, ws.UTC().Format(time.RFC3339), we.UTC().Format(time.RFC3339))
}

// summarizeCloudSQLCPU is the file the step directory keeps: the window, the mean and peak of the
// aligned fractions, each of them as cores of the stated vCPU count, and the same window's memory
// in bytes, as a share of the instance's RAM where the quota series answered.
func summarizeCloudSQLCPU(project, instance string, ws, we time.Time, vcpus float64, points, memory, quota []cloudsqlPoint) cloudsqlCPUFile {
	f := cloudsqlCPUFile{
		Project:          project,
		Instance:         instance,
		DatabaseID:       cloudsqlDatabaseID(project, instance),
		Metric:           cloudsqlCPUMetric,
		WindowStart:      ws,
		WindowEnd:        we,
		WindowS:          we.Sub(ws).Seconds(),
		AlignmentPeriodS: cloudsqlAlignment.Seconds(),
		Samples:          len(points),
		LimitCores:       vcpus,
		Points:           make([]cloudsqlCPUPoint, 0, len(points)),
		MemoryMetric:     cloudsqlMemoryMetric,
		MemorySamples:    len(memory),
		MemoryPoints:     make([]cloudsqlMemoryPoint, 0, len(memory)),
	}
	sum := 0.0
	for _, p := range points {
		sum += p.Value
		f.UtilizationMax = max(f.UtilizationMax, p.Value)
		f.Points = append(f.Points, cloudsqlCPUPoint{At: p.At, Utilization: p.Value, Cores: p.Value * vcpus})
	}
	f.UtilizationMean = sum / float64(len(points))
	f.CoresMean, f.CoresPeak60s = f.UtilizationMean*vcpus, f.UtilizationMax*vcpus
	bytes := 0.0
	for _, p := range memory {
		bytes += p.Value
		f.MemoryBytesMax = max(f.MemoryBytesMax, p.Value)
		f.MemoryPoints = append(f.MemoryPoints, cloudsqlMemoryPoint{At: p.At, Bytes: p.Value})
	}
	f.MemoryBytesMean = bytes / float64(len(memory))
	if len(quota) > 0 {
		limit := 0.0
		for _, p := range quota {
			limit = max(limit, p.Value)
		}
		f.MemoryLimitBytes, f.MemoryUtilizationMean = new(limit), new(f.MemoryBytesMean/limit)
	}
	return f
}

func runCloudSQLCPU(args []string) error {
	fs := flag.NewFlagSet("cloudsql-cpu", flag.ExitOnError)
	step := fs.String("step", "", "the step directory whose meta.json gives the window (required)")
	project := fs.String("project", "", "the Google project the instance lives in (required)")
	instance := fs.String("instance", "", "the Cloud SQL instance's name, which resource.labels.database_id carries as <project>:<instance> (required)")
	out := fs.String("out", "", "where the JSON is written (default <step>/cloudsql-cpu.json)")
	vcpus := fs.Float64("vcpus", cloudsqlDefaultVCPUs, "the instance's vCPUs, which the utilization fraction is of (production's db-custom-16-61440 has 16)")
	api := fs.String("api", cloudsqlMonitoringBase, "Cloud Monitoring's base URL, for a test server")
	tokenURL := fs.String("token-url", cloudsqlTokenURL, "where the access token is read, for a test server")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: loadgen cloudsql-cpu -step <step dir> -project <project> -instance <name> [-out <file>] [-vcpus 16]")
		fmt.Fprintln(fs.Output(), "Reads "+cloudsqlCPUMetric+" and "+cloudsqlMemoryMetric+" over the step's")
		fmt.Fprintln(fs.Output(), "window, aligned to 60 s, with the metadata server's access token, and writes their means")
		fmt.Fprintln(fs.Output(), "and every sample as cloudsql-cpu.json.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case *step == "":
		return errors.New("-step is required: the step directory whose meta.json gives the window")
	case *project == "":
		return errors.New("-project is required: the Google project the Cloud SQL instance lives in")
	case *instance == "":
		return errors.New("-instance is required: the Cloud SQL instance's name")
	case *vcpus <= 0:
		return fmt.Errorf("-vcpus must be above 0, not %g", *vcpus)
	case *api == "":
		return errors.New("-api is required: Cloud Monitoring's base URL")
	case *tokenURL == "":
		return errors.New("-token-url is required: where the access token is read")
	}
	if *out == "" {
		*out = filepath.Join(*step, "cloudsql-cpu.json")
	}
	ws, we, _, err := readStepWindow(*step)
	if err != nil {
		return err
	}
	ctx := context.Background()
	token, err := cloudsqlMetadataToken(ctx, shortClient, *tokenURL)
	if err != nil {
		return err
	}
	databaseID := cloudsqlDatabaseID(*project, *instance)
	points, err := readCloudSQLSeries(ctx, shortClient, *api, token, *project, cloudsqlCPUMetric, databaseID, ws, we)
	if err != nil {
		return err
	}
	if len(points) == 0 {
		return errNoCloudSQLSample(cloudsqlCPUMetric, databaseID, ws, we)
	}
	memory, err := readCloudSQLSeries(ctx, shortClient, *api, token, *project, cloudsqlMemoryMetric, databaseID, ws, we)
	if err != nil {
		return err
	}
	if len(memory) == 0 {
		return errNoCloudSQLSample(cloudsqlMemoryMetric, databaseID, ws, we)
	}
	quota, err := readCloudSQLSeries(ctx, shortClient, *api, token, *project, cloudsqlMemoryQuotaMetric, databaseID, ws, we)
	if err != nil {
		return err
	}
	f := summarizeCloudSQLCPU(*project, *instance, ws, we, *vcpus, points, memory, quota)
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "loadgen cloudsql-cpu: wrote %s: %d samples of %s, mean %.3f of %g vCPUs (%.2f cores), peak %.2f cores; memory mean %.0f MiB over %d samples\n",
		*out, f.Samples, databaseID, f.UtilizationMean, f.LimitCores, f.CoresMean, f.CoresPeak60s, mib(f.MemoryBytesMean), f.MemorySamples)
	return nil
}
