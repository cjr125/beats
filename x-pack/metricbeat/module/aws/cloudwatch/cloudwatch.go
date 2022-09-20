// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package cloudwatch

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	resourcegroupstaggingapitypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"

	"github.com/elastic/beats/v7/libbeat/common"
	"github.com/elastic/beats/v7/metricbeat/mb"
	"github.com/elastic/beats/v7/x-pack/metricbeat/module/aws"
	"github.com/elastic/elastic-agent-libs/logp"
)

var (
	metricsetName          = "cloudwatch"
	metricNameIdx          = 0
	namespaceIdx           = 1
	statisticIdx           = 2
	identifierNameIdx      = 3
	identifierValueIdx     = 4
	defaultStatistics      = []string{"Average", "Maximum", "Minimum", "Sum", "SampleCount"}
	labelSeparator         = "|"
	dimensionSeparator     = ","
	dimensionValueWildcard = "*"
)

// init registers the MetricSet with the central registry as soon as the program
// starts. The New function will be called later to instantiate an instance of
// the MetricSet for each host defined in the module's configuration. After the
// MetricSet has been created then Fetch will begin to be called periodically.
func init() {
	mb.Registry.MustAddMetricSet(aws.ModuleName, metricsetName, New,
		mb.DefaultMetricSet(),
	)
}

// MetricSet holds any configuration or state information. It must implement
// the mb.MetricSet interface. And this is best achieved by embedding
// mb.BaseMetricSet because it implements all of the required mb.MetricSet
// interface methods except for Fetch.
type MetricSet struct {
	*aws.MetricSet
	logger            *logp.Logger
	CloudwatchConfigs []Config `config:"metrics" validate:"nonzero,required"`
}

// Dimension holds name and value for cloudwatch metricset dimension config.
type Dimension struct {
	Name  string `config:"name" validate:"nonzero"`
	Value string `config:"value" validate:"nonzero"`
}

// Config holds a configuration specific for cloudwatch metricset.
type Config struct {
	Namespace    string      `config:"namespace" validate:"nonzero,required"`
	MetricName   []string    `config:"name"`
	Dimensions   []Dimension `config:"dimensions"`
	ResourceType string      `config:"resource_type"`
	Statistic    []string    `config:"statistic"`
}

type MetricsWithStatistics struct {
	CloudwatchMetric types.Metric
	Statistic        []string
}

type ListMetricWithDetail struct {
	MetricsWithStats    []MetricsWithStatistics
	ResourceTypeFilters map[string][]aws.Tag
}

// namespaceDetail collects configuration details for each namespace
type NamespaceDetail struct {
	ResourceTypeFilter string
	Names              []string
	Tags               []aws.Tag
	Statistics         []string
	Dimensions         []types.Dimension
}

// New creates a new instance of the MetricSet. New is responsible for unpacking
// any MetricSet specific configuration options if there are any.
func New(base mb.BaseMetricSet) (mb.MetricSet, error) {
	logger := logp.NewLogger(metricsetName)
	metricSet, err := aws.NewMetricSet(base)
	if err != nil {
		return nil, fmt.Errorf("error creating aws metricset: %w", err)
	}

	config := struct {
		CloudwatchMetrics []Config `config:"metrics" validate:"nonzero,required"`
	}{}

	err = base.Module().UnpackConfig(&config)
	if err != nil {
		return nil, fmt.Errorf("error unpack raw module config using UnpackConfig: %w", err)
	}

	logger.Debugf("cloudwatch config = %s", config)
	if len(config.CloudwatchMetrics) == 0 {
		return nil, fmt.Errorf("metrics in config is missing: %w", err)
	}

	return &MetricSet{
		MetricSet:         metricSet,
		logger:            logger,
		CloudwatchConfigs: config.CloudwatchMetrics,
	}, nil
}

// Fetch methods implements the data gathering and data conversion to the right
// format. It publishes the event which is then forwarded to the output. In case
// of an error set the Error field of mb.Event or simply call report.Error().
func (m *MetricSet) Fetch(report mb.ReporterV2) error {
	// Get startTime and endTime
	startTime, endTime := aws.GetStartTimeEndTime(time.Now(), m.Period, m.Latency)
	m.Logger().Debugf("startTime = %s, endTime = %s", startTime, endTime)

	// Check statistic method in config
	err := m.checkStatistics()
	if err != nil {
		return fmt.Errorf("checkStatistics failed: %w", err)
	}

	// Get listMetricDetailTotal and namespaceDetailTotal from configuration
	listMetricDetailTotal, namespaceDetailTotal := m.readCloudwatchConfig()
	m.logger.Debugf("listMetricDetailTotal = %s", listMetricDetailTotal)
	m.logger.Debugf("namespaceDetailTotal = %s", namespaceDetailTotal)

	var config aws.Config
	err = m.Module().UnpackConfig(&config)
	if err != nil {
		return err
	}

	// Create events based on listMetricDetailTotal from configuration
	if len(listMetricDetailTotal.MetricsWithStats) != 0 {
		for _, regionName := range m.MetricSet.RegionsList {
			//m.logger.Debugf("Collecting metrics from AWS region %s", regionName)
			beatsConfig := m.MetricSet.AwsConfig.Copy()
			beatsConfig.Region = regionName

			svcCloudwatch, svcResourceAPI, err := m.createAwsRequiredClients(beatsConfig, regionName, config)
			if err != nil {
				m.Logger().Warn("skipping metrics list from region '%s'", regionName)
			}

			eventsWithIdentifier, err := m.createEvents(svcCloudwatch, svcResourceAPI, listMetricDetailTotal.MetricsWithStats, listMetricDetailTotal.ResourceTypeFilters, regionName, startTime, endTime)
			if err != nil {
				return fmt.Errorf("createEvents failed for region %s: %w", regionName, err)
			}

			m.logger.Debugf("Collected metrics of metrics = %d", len(eventsWithIdentifier))

			for _, event := range eventsWithIdentifier {
				report.Event(event)
			}
		}
	}

	// Create events based on namespaceDetailTotal from configuration
	for _, regionName := range m.MetricSet.RegionsList {
		m.logger.Debugf("Collecting metrics from AWS region %s", regionName)
		beatsConfig := m.MetricSet.AwsConfig.Copy()
		beatsConfig.Region = regionName

		svcCloudwatch, svcResourceAPI, err := m.createAwsRequiredClients(beatsConfig, regionName, config)
		if err != nil {
			m.Logger().Warn("skipping metrics list from region '%s'", regionName)
		}
		for namespace, namespaceDetails := range namespaceDetailTotal {
			m.logger.Debugf("Collected metrics from namespace %s", namespace)

			listMetricsOutput, err := aws.GetListMetricsOutput(namespace, regionName, svcCloudwatch)
			if err != nil {
				m.logger.Info(err.Error())
				continue
			}

			if len(listMetricsOutput) == 0 {
				continue
			}

			// filter listMetricsOutput by detailed configuration per each namespace
			filteredMetricWithStatsTotal := FilterListMetricsOutput(listMetricsOutput, namespaceDetails)
			// get resource type filters and tags filters for each namespace
			resourceTypeTagFilters := ConstructTagsFilters(namespaceDetails)

			eventsWithIdentifier, err := m.createEvents(svcCloudwatch, svcResourceAPI, filteredMetricWithStatsTotal, resourceTypeTagFilters, regionName, startTime, endTime)
			if err != nil {
				return fmt.Errorf("createEvents failed for region %s: %w", regionName, err)
			}

			m.logger.Debugf("Collected number of metrics = %d", len(eventsWithIdentifier))

			events, err := addMetadata(namespace, regionName, beatsConfig, config.AWSConfig.FIPSEnabled, eventsWithIdentifier)
			if err != nil {
				// TODO What to do if add metadata fails? I guess to continue, probably we have an 90% of reliable data
				m.Logger().Warn("could not add metadata to events: %w", err)
			}

			for _, event := range events {
				report.Event(event)
			}
		}
	}
	return nil
}

// createAwsRequiredClients will return the two necessary client instances to do Metric requests to the AWS API
func (m *MetricSet) createAwsRequiredClients(beatsConfig awssdk.Config, regionName string, config aws.Config) (*cloudwatch.Client, *resourcegroupstaggingapi.Client, error) {
	m.logger.Debugf("Collecting metrics from AWS region %s", regionName)

	svcCloudwatchClient := cloudwatch.NewFromConfig(beatsConfig, func(o *cloudwatch.Options) {
		if config.AWSConfig.FIPSEnabled {
			o.EndpointOptions.UseFIPSEndpoint = awssdk.FIPSEndpointStateEnabled
		}

	})

	svcResourceAPIClient := resourcegroupstaggingapi.NewFromConfig(beatsConfig, func(o *resourcegroupstaggingapi.Options) {
		if config.AWSConfig.FIPSEnabled {
			o.EndpointOptions.UseFIPSEndpoint = awssdk.FIPSEndpointStateEnabled
		}
	})

	return svcCloudwatchClient, svcResourceAPIClient, nil
}

// filterListMetricsOutput compares config details with listMetricsOutput and filter out the ones don't match
func FilterListMetricsOutput(listMetricsOutput []types.Metric, namespaceDetails []NamespaceDetail) []MetricsWithStatistics {
	var filteredMetricWithStatsTotal []MetricsWithStatistics
	for _, listMetric := range listMetricsOutput {
		for _, configPerNamespace := range namespaceDetails {
			if configPerNamespace.Names != nil && configPerNamespace.Dimensions == nil {
				// if metric names are given in config but no dimensions, filter
				// out the metrics with other names
				if exists, _ := aws.StringInSlice(*listMetric.MetricName, configPerNamespace.Names); !exists {
					continue
				}
				filteredMetricWithStatsTotal = append(filteredMetricWithStatsTotal,
					MetricsWithStatistics{
						CloudwatchMetric: listMetric,
						Statistic:        configPerNamespace.Statistics,
					})

			} else if configPerNamespace.Names == nil && configPerNamespace.Dimensions != nil {
				// if metric names are not given in config but dimensions are
				// given, only keep the metrics with matching dimensions
				if !CompareAWSDimensions(listMetric.Dimensions, configPerNamespace.Dimensions) {
					continue
				}
				filteredMetricWithStatsTotal = append(filteredMetricWithStatsTotal,
					MetricsWithStatistics{
						CloudwatchMetric: listMetric,
						Statistic:        configPerNamespace.Statistics,
					})
			} else if configPerNamespace.Names != nil && configPerNamespace.Dimensions != nil {
				if exists, _ := aws.StringInSlice(*listMetric.MetricName, configPerNamespace.Names); !exists {
					continue
				}
				if !CompareAWSDimensions(listMetric.Dimensions, configPerNamespace.Dimensions) {
					continue
				}
				filteredMetricWithStatsTotal = append(filteredMetricWithStatsTotal,
					MetricsWithStatistics{
						CloudwatchMetric: listMetric,
						Statistic:        configPerNamespace.Statistics,
					})
			} else {
				// if no metric name and no dimensions given, then keep all listMetricsOutput
				filteredMetricWithStatsTotal = append(filteredMetricWithStatsTotal,
					MetricsWithStatistics{
						CloudwatchMetric: listMetric,
						Statistic:        configPerNamespace.Statistics,
					})
			}
		}
	}
	return filteredMetricWithStatsTotal
}

// Collect resource type filters and tag filters from config for cloudwatch
func ConstructTagsFilters(namespaceDetails []NamespaceDetail) map[string][]aws.Tag {
	resourceTypeTagFilters := map[string][]aws.Tag{}
	for _, configPerNamespace := range namespaceDetails {
		if configPerNamespace.ResourceTypeFilter != "" {
			if _, ok := resourceTypeTagFilters[configPerNamespace.ResourceTypeFilter]; ok {
				resourceTypeTagFilters[configPerNamespace.ResourceTypeFilter] = append(resourceTypeTagFilters[configPerNamespace.ResourceTypeFilter], configPerNamespace.Tags...)
			} else {
				resourceTypeTagFilters[configPerNamespace.ResourceTypeFilter] = configPerNamespace.Tags
			}
		}
	}
	return resourceTypeTagFilters
}

func (m *MetricSet) checkStatistics() error {
	for _, config := range m.CloudwatchConfigs {
		for _, stat := range config.Statistic {
			if _, ok := StatisticLookup(stat); !ok {
				return fmt.Errorf("statistic method specified is not valid: %s", stat)
			}
		}
	}
	return nil
}

func (m *MetricSet) readCloudwatchConfig() (ListMetricWithDetail, map[string][]NamespaceDetail) {
	var listMetricDetailTotal ListMetricWithDetail
	namespaceDetailTotal := map[string][]NamespaceDetail{}
	var metricsWithStatsTotal []MetricsWithStatistics
	resourceTypesWithTags := map[string][]aws.Tag{}

	for _, config := range m.CloudwatchConfigs {
		// If there is no statistic method specified, then use the default.
		if config.Statistic == nil {
			config.Statistic = defaultStatistics
		}

		var cloudwatchDimensions []types.Dimension
		for _, dim := range config.Dimensions {
			name := dim.Name
			value := dim.Value
			cloudwatchDimensions = append(cloudwatchDimensions, types.Dimension{
				Name:  &name,
				Value: &value,
			})
		}
		// if any Dimension value contains wildcard, then compare dimensions with
		// listMetrics result in filterListMetricsOutput
		if config.MetricName != nil && config.Dimensions != nil &&
			!ConfigDimensionValueContainsWildcard(config.Dimensions) {
			namespace := config.Namespace
			for i := range config.MetricName {
				metricsWithStats := MetricsWithStatistics{
					CloudwatchMetric: types.Metric{
						Namespace:  &namespace,
						MetricName: &config.MetricName[i],
						Dimensions: cloudwatchDimensions,
					},
					Statistic: config.Statistic,
				}
				metricsWithStatsTotal = append(metricsWithStatsTotal, metricsWithStats)
			}

			if config.ResourceType != "" {
				resourceTypesWithTags[config.ResourceType] = m.MetricSet.TagsFilter
			}
			continue
		}

		configPerNamespace := NamespaceDetail{
			Names:              config.MetricName,
			Tags:               m.MetricSet.TagsFilter,
			Statistics:         config.Statistic,
			ResourceTypeFilter: config.ResourceType,
			Dimensions:         cloudwatchDimensions,
		}

		namespaceDetailTotal[config.Namespace] = append(namespaceDetailTotal[config.Namespace], configPerNamespace)
	}

	listMetricDetailTotal.ResourceTypeFilters = resourceTypesWithTags
	listMetricDetailTotal.MetricsWithStats = metricsWithStatsTotal
	return listMetricDetailTotal, namespaceDetailTotal
}

func CreateMetricDataQueries(listMetricsTotal []MetricsWithStatistics, period time.Duration) []types.MetricDataQuery {
	var metricDataQueries []types.MetricDataQuery
	for i, listMetric := range listMetricsTotal {
		for j, statistic := range listMetric.Statistic {
			stat := statistic
			metric := listMetric.CloudwatchMetric
			label := ConstructLabel(listMetric.CloudwatchMetric, statistic)
			periodInSec := int32(period.Seconds())

			id := "cw" + strconv.Itoa(i) + "stats" + strconv.Itoa(j)
			metricDataQueries = append(metricDataQueries, types.MetricDataQuery{
				Id: &id,
				MetricStat: &types.MetricStat{
					Period: &periodInSec,
					Stat:   &stat,
					Metric: &metric,
				},
				Label: &label,
			})
		}
	}
	return metricDataQueries
}

func ConstructLabel(metric types.Metric, statistic string) string {
	// label = metricName + namespace + statistic + dimKeys + dimValues
	label := *metric.MetricName + labelSeparator + *metric.Namespace + labelSeparator + statistic
	dimNames := ""
	dimValues := ""
	for i, dim := range metric.Dimensions {
		dimNames += *dim.Name
		dimValues += *dim.Value
		if i != len(metric.Dimensions)-1 {
			dimNames += dimensionSeparator
			dimValues += dimensionSeparator
		}
	}

	if dimNames != "" && dimValues != "" {
		label += labelSeparator + dimNames
		label += labelSeparator + dimValues
	}
	return label
}

func StatisticLookup(stat string) (string, bool) {
	statisticLookupTable := map[string]string{
		"Average":     "avg",
		"Sum":         "sum",
		"Maximum":     "max",
		"Minimum":     "min",
		"SampleCount": "count",
	}
	statMethod, ok := statisticLookupTable[stat]
	if !ok {
		ok = strings.HasPrefix(stat, "p")
		statMethod = stat
	}
	return statMethod, ok
}

func GenerateFieldName(namespace string, labels []string) string {
	stat := labels[statisticIdx]
	// Check if statistic method is one of Sum, SampleCount, Minimum, Maximum, Average
	// With checkStatistics function, no need to check bool return value here
	statMethod, _ := StatisticLookup(stat)
	// By default, replace dot "." using underscore "_" for metric names
	return "aws." + StripNamespace(namespace) + ".metrics." + common.DeDot(labels[metricNameIdx]) + "." + statMethod
}

// StripNamespace converts Cloudwatch namespace into the root field we will use for metrics
// example AWS/EC2 -> ec2
func StripNamespace(namespace string) string {
	parts := strings.Split(namespace, "/")
	return strings.ToLower(parts[len(parts)-1])
}

func InsertRootFields(event mb.Event, metricValue float64, labels []string) mb.Event {
	namespace := labels[namespaceIdx]
	_, _ = event.RootFields.Put(GenerateFieldName(namespace, labels), metricValue)
	_, _ = event.RootFields.Put("aws.cloudwatch.namespace", namespace)
	if len(labels) == 3 {
		return event
	}

	dimNames := strings.Split(labels[identifierNameIdx], ",")
	dimValues := strings.Split(labels[identifierValueIdx], ",")
	for i := 0; i < len(dimNames); i++ {
		_, _ = event.RootFields.Put("aws.dimensions."+dimNames[i], dimValues[i])
	}
	return event
}

func (m *MetricSet) createEvents(svcCloudwatch cloudwatch.GetMetricDataAPIClient, svcResourceAPI resourcegroupstaggingapi.GetResourcesAPIClient, listMetricWithStatsTotal []MetricsWithStatistics, resourceTypeTagFilters map[string][]aws.Tag, regionName string, startTime time.Time, endTime time.Time) (map[string]mb.Event, error) {
	// Initialize events for each identifier.
	events := map[string]mb.Event{}

	// Construct metricDataQueries
	metricDataQueries := CreateMetricDataQueries(listMetricWithStatsTotal, m.Period)
	m.logger.Debugf("Number of MetricDataQueries = %d", len(metricDataQueries))
	if len(metricDataQueries) == 0 {
		return events, nil
	}

	// Use metricDataQueries to make GetMetricData API calls
	metricDataResults, err := aws.GetMetricDataResults(metricDataQueries, svcCloudwatch, startTime, endTime)
	m.logger.Debugf("Number of metricDataResults = %d", len(metricDataResults))
	if err != nil {
		return events, fmt.Errorf("getMetricDataResults failed: %w", err)
	}

	// Find a timestamp for all metrics in output
	timestamp := aws.FindTimestamp(metricDataResults)
	if timestamp.IsZero() {
		return nil, nil
	}

	// Create events when there is no tags_filter or resource_type specified.
	if len(resourceTypeTagFilters) == 0 {
		for _, metricDataResult := range metricDataResults {
			if len(metricDataResult.Values) == 0 {
				continue
			}

			exists, timestampIdx := aws.CheckTimestampInArray(timestamp, metricDataResult.Timestamps)
			if exists {
				labels := strings.Split(*metricDataResult.Label, labelSeparator)
				if len(labels) != 5 {
					// when there is no identifier value in label, use region+accountID+namespace instead
					identifier := regionName + m.AccountID + labels[namespaceIdx]
					if _, ok := events[identifier]; !ok {
						events[identifier] = aws.InitEvent(regionName, m.AccountName, m.AccountID, timestamp)
					}
					events[identifier] = InsertRootFields(events[identifier], metricDataResult.Values[timestampIdx], labels)
					continue
				}

				identifierValue := labels[identifierValueIdx]
				if _, ok := events[identifierValue]; !ok {
					events[identifierValue] = aws.InitEvent(regionName, m.AccountName, m.AccountID, timestamp)
				}
				events[identifierValue] = InsertRootFields(events[identifierValue], metricDataResult.Values[timestampIdx], labels)
			}
		}
		return events, nil
	}

	// Create events with tags
	for resourceType, tagsFilter := range resourceTypeTagFilters {
		m.logger.Debugf("resourceType = %s", resourceType)
		m.logger.Debugf("tagsFilter = %s", tagsFilter)
		resourceTagMap, err := aws.GetResourcesTags(svcResourceAPI, []string{resourceType})
		if err != nil {
			// If GetResourcesTags failed, continue report event just without tags.
			m.logger.Info(fmt.Errorf("getResourcesTags failed, skipping region %s: %w", regionName, err))
		}

		if len(tagsFilter) != 0 && len(resourceTagMap) == 0 {
			continue
		}

		// filter resourceTagMap
		for identifier, tags := range resourceTagMap {
			if exists := aws.CheckTagFiltersExist(tagsFilter, tags); !exists {
				m.logger.Debugf("In region %s, service %s tags does not match tags_filter", regionName, identifier)
				delete(resourceTagMap, identifier)
				continue
			}
			m.logger.Debugf("In region %s, service %s tags match tags_filter", regionName, identifier)
		}

		for _, output := range metricDataResults {
			if len(output.Values) == 0 {
				continue
			}

			exists, timestampIdx := aws.CheckTimestampInArray(timestamp, output.Timestamps)
			if exists {
				labels := strings.Split(*output.Label, labelSeparator)
				if len(labels) != 5 {
					// if there is no tag in labels but there is a tagsFilter, then no event should be reported.
					if len(tagsFilter) != 0 {
						continue
					}

					// when there is no identifier value in label, use region+accountID+namespace instead
					identifier := regionName + m.AccountID + labels[namespaceIdx]
					if _, ok := events[identifier]; !ok {
						events[identifier] = aws.InitEvent(regionName, m.AccountName, m.AccountID, timestamp)
					}
					events[identifier] = InsertRootFields(events[identifier], output.Values[timestampIdx], labels)
					continue
				}

				identifierValue := labels[identifierValueIdx]
				if _, ok := events[identifierValue]; !ok {
					// when tagsFilter is not empty but no entry in
					// resourceTagMap for this identifier, do not initialize
					// an event for this identifier.
					if len(tagsFilter) != 0 && resourceTagMap[identifierValue] == nil {
						continue
					}
					events[identifierValue] = aws.InitEvent(regionName, m.AccountName, m.AccountID, timestamp)
				}
				events[identifierValue] = InsertRootFields(events[identifierValue], output.Values[timestampIdx], labels)

				// add tags to event based on identifierValue
				InsertTags(events, identifierValue, resourceTagMap)
			}
		}
	}
	return events, nil
}

func ConfigDimensionValueContainsWildcard(dim []Dimension) bool {
	for i := range dim {
		if dim[i].Value == dimensionValueWildcard {
			return true
		}
	}
	return false
}

func CompareAWSDimensions(dim1 []types.Dimension, dim2 []types.Dimension) bool {
	if len(dim1) != len(dim2) {
		return false
	}

	var dim1NameToValue = make(map[string]string, len(dim1))
	var dim2NameToValue = make(map[string]string, len(dim1))

	for i := range dim2 {
		dim1NameToValue[*dim1[i].Name] = *dim1[i].Value
		dim2NameToValue[*dim2[i].Name] = *dim2[i].Value
	}
	for name, v1 := range dim1NameToValue {
		v2, exists := dim2NameToValue[name]
		if exists && v2 == dimensionValueWildcard {
			// wildcard can represent any value, so we set the
			// dimension name with value in CloudWatch ListMetircs result,
			// then the compare result is true
			dim2NameToValue[name] = v1
		}
	}
	return reflect.DeepEqual(dim1NameToValue, dim2NameToValue)
}

func InsertTags(events map[string]mb.Event, identifier string, resourceTagMap map[string][]resourcegroupstaggingapitypes.Tag) {
	// Check if identifier includes dimensionSeparator (comma in this case),
	// split the identifier and check for each sub-identifier.
	// For example, identifier might be [storageType, s3BucketName].
	// And tags are only store under s3BucketName in resourceTagMap.
	subIdentifiers := strings.Split(identifier, dimensionSeparator)
	for _, v := range subIdentifiers {
		tags := resourceTagMap[v]
		// some metric dimension values are arn format, eg: AWS/DDOS namespace metric
		if len(tags) == 0 && strings.HasPrefix(v, "arn:") {
			resourceID, err := aws.FindShortIdentifierFromARN(v)
			if err == nil {
				tags = resourceTagMap[resourceID]
			}
		}
		if len(tags) != 0 {
			// By default, replace dot "." using underscore "_" for tag keys.
			// Note: tag values are not dedotted.
			for _, tag := range tags {
				_, _ = events[identifier].RootFields.Put("aws.tags."+common.DeDot(*tag.Key), *tag.Value)
			}
			continue
		}
	}
}
