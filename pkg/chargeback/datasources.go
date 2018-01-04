package chargeback

import (
	"fmt"

	log "github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"

	cbTypes "github.com/coreos-inc/kube-chargeback/pkg/apis/chargeback/v1alpha1"
	"github.com/coreos-inc/kube-chargeback/pkg/aws"
	cbListers "github.com/coreos-inc/kube-chargeback/pkg/generated/listers/chargeback/v1alpha1"
	"github.com/coreos-inc/kube-chargeback/pkg/hive"
)

func (c *Chargeback) runReportDataSourceWorker() {
	logger := c.logger.WithField("component", "reportDataSourceWorker")
	logger.Infof("ReportDataSource worker started")
	for c.processReportDataSource(logger) {

	}
}

func (c *Chargeback) processReportDataSource(logger log.FieldLogger) bool {
	key, quit := c.informers.reportDataSourceQueue.Get()
	if quit {
		return false
	}
	defer c.informers.reportDataSourceQueue.Done(key)

	logger = logger.WithFields(newLogIdentifier())
	err := c.syncReportDataSource(logger, key.(string))
	c.handleErr(logger, err, "ReportDataSource", key, c.informers.reportDataSourceQueue)
	return true
}

func (c *Chargeback) syncReportDataSource(logger log.FieldLogger, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logger.WithError(err).Errorf("invalid resource key :%s", key)
		return nil
	}

	logger = logger.WithField("datasource", name)
	reportDataSource, err := c.informers.reportDataSourceLister.ReportDataSources(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Infof("ReportDataSource %s does not exist anymore", key)
			return nil
		}
		return err
	}

	logger.Infof("syncing reportDataSource %s", reportDataSource.GetName())
	err = c.handleReportDataSource(logger, reportDataSource)
	if err != nil {
		logger.WithError(err).Errorf("error syncing reportDataSource %s", reportDataSource.GetName())
		return err
	}
	logger.Infof("successfully synced reportDataSource %s", reportDataSource.GetName())
	return nil
}

func (c *Chargeback) handleReportDataSource(logger log.FieldLogger, dataSource *cbTypes.ReportDataSource) error {
	dataSource = dataSource.DeepCopy()
	if dataSource.TableName == "" {
		logger.Infof("new dataSource discovered")
	} else {
		logger.Infof("existing dataSource discovered, tableName: %s", dataSource.TableName)
		return nil
	}

	switch {
	case dataSource.Spec.Promsum != nil:
		return c.handlePromsumDataSource(logger, dataSource)
	case dataSource.Spec.AWSBilling != nil:
		return c.handleAWSBillingDataSource(logger, dataSource)
	default:
		return fmt.Errorf("datasource %s: improperly configured missing promsum or awsBilling configuration", dataSource.Name)
	}
}

func (c *Chargeback) handlePromsumDataSource(logger log.FieldLogger, dataSource *cbTypes.ReportDataSource) error {
	storage := dataSource.Spec.Promsum.Storage
	tableName := dataSourceTableName(dataSource.Name)

	var storageSpec cbTypes.StorageLocationSpec
	// Nothing specified, try to use default storage location
	if storage == nil || (storage.StorageSpec == nil && storage.StorageLocationName == "") {
		logger.Info("reportDataSource does not have a storageSpec or storageLocationName set, using default storage location")
		storageLocation, err := c.getDefaultStorageLocation(c.informers.storageLocationLister)
		if err != nil {
			return err
		}
		if storageLocation == nil {
			return fmt.Errorf("invalid promsum DataSource, no storageSpec or storageLocationName and cluster has no default StorageLocation")
		}

		storageSpec = storageLocation.Spec
	} else if storage.StorageLocationName != "" { // Specific storage location specified
		logger.Infof("reportDataSource configured to use StorageLocation %s", storage.StorageLocationName)
		storageLocation, err := c.informers.storageLocationLister.StorageLocations(c.namespace).Get(storage.StorageLocationName)
		if err != nil {
			return err
		}
		storageSpec = storageLocation.Spec
	} else if storage.StorageSpec != nil { // Storage location is inlined in the datasource
		storageSpec = *storage.StorageSpec
	}

	var createTableParams hive.CreateTableParameters
	var err error
	if storageSpec.Local != nil {
		logger.Debugf("creating local table %s", tableName)
		createTableParams, err = hive.CreateLocalPromsumTable(c.hiveQueryer, tableName)
		if err != nil {
			return err
		}
	} else if storageSpec.S3 != nil {
		logger.Debugf("creating table %s backed by s3 bucket %s at prefix %s", tableName, storageSpec.S3.Bucket, storageSpec.S3.Prefix)
		createTableParams, err = hive.CreateS3PromsumTable(c.hiveQueryer, tableName, storageSpec.S3.Bucket, storageSpec.S3.Prefix)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("storage incorrectly configured on datasource %s", dataSource.Name)
	}

	logger.Debugf("creating presto table CR for table %q", tableName)
	err = c.createPrestoTableCR(dataSource, createTableParams)
	if err != nil {
		logger.WithError(err).Errorf("failed to create PrestoTable CR %q", tableName)
		return err
	}

	logger.Debugf("successfully created table %s", tableName)

	return c.updateDataSourceTableName(logger, dataSource, tableName)
}

func (c *Chargeback) getDefaultStorageLocation(lister cbListers.StorageLocationLister) (*cbTypes.StorageLocation, error) {
	storageLocations, err := c.informers.storageLocationLister.StorageLocations(c.namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	var defaultStorageLocations []*cbTypes.StorageLocation

	for _, storageLocation := range storageLocations {
		if storageLocation.Annotations[cbTypes.IsDefaultStorageLocationAnnotation] == "true" {
			defaultStorageLocations = append(defaultStorageLocations, storageLocation)
		}
	}

	if len(defaultStorageLocations) == 0 {
		return nil, nil
	}

	if len(defaultStorageLocations) > 1 {
		c.logger.Infof("getDefaultStorageLocation %s default storageLocations found", len(defaultStorageLocations))
		return nil, fmt.Errorf("%d defaultStorageLocations were found", len(defaultStorageLocations))
	}

	return defaultStorageLocations[0], nil

}

func (c *Chargeback) handleAWSBillingDataSource(logger log.FieldLogger, dataSource *cbTypes.ReportDataSource) error {
	source := dataSource.Spec.AWSBilling.Source
	if source == nil {
		return fmt.Errorf("datasource %q: improperly configured datasource, source is empty", dataSource.Name)
	}

	manifestRetriever, err := aws.NewManifestRetriever(source.Bucket, source.Prefix)
	if err != nil {
		return err
	}

	manifests, err := manifestRetriever.RetrieveManifests()
	if err != nil {
		return err
	}

	if len(manifests) == 0 {
		logger.Warnf("datasource %q has no report manifests in it's bucket, the first report has likely not been generated yet", dataSource.Name)
		return nil
	}

	tableName := dataSourceTableName(dataSource.Name)
	logger.Debugf("creating AWS Billing DataSource table %s pointing to s3 bucket %s at prefix %s", tableName, source.Bucket, source.Prefix)
	createTableParams, err := hive.CreateAWSUsageTable(c.hiveQueryer, tableName, source.Bucket, source.Prefix, manifests)
	if err != nil {
		return err
	}

	logger.Debugf("creating presto table CR for table %q", tableName)
	err = c.createPrestoTableCR(dataSource, createTableParams)
	if err != nil {
		logger.WithError(err).Errorf("failed to create PrestoTable CR %q", tableName)
		return err
	}

	logger.Debugf("successfully created AWS Billing DataSource table %s pointing to s3 bucket %s at prefix %s", tableName, source.Bucket, source.Prefix)

	err = c.updateDataSourceTableName(logger, dataSource, tableName)
	if err != nil {
		return err
	}

	c.prestoTablePartitionQueue <- dataSource
	return nil
}

func (c *Chargeback) createPrestoTableCR(dataSource *cbTypes.ReportDataSource, params hive.CreateTableParameters) error {
	prestoTableCR := cbTypes.PrestoTable{
		TypeMeta: meta.TypeMeta{
			Kind:       "PrestoTable",
			APIVersion: dataSource.APIVersion,
		},
		ObjectMeta: meta.ObjectMeta{
			Name:      dataSourceNameToPrestoTableName(dataSource.Name),
			Namespace: dataSource.Namespace,
			Labels:    dataSource.Labels,
			OwnerReferences: []meta.OwnerReference{
				{
					APIVersion: dataSource.APIVersion,
					Kind:       dataSource.Kind,
					Name:       dataSource.Name,
					UID:        dataSource.UID,
				},
			},
		},
		State: cbTypes.PrestoTableState{
			CreationParameters: cbTypes.PrestoTableCreationParameters{
				TableName:    params.Name,
				Location:     params.Location,
				SerdeFmt:     params.SerdeFmt,
				Format:       params.Format,
				SerdeProps:   params.SerdeProps,
				External:     params.External,
				IgnoreExists: params.IgnoreExists,
			},
		},
	}
	for _, col := range params.Columns {
		prestoTableCR.State.CreationParameters.Columns = append(prestoTableCR.State.CreationParameters.Columns, cbTypes.PrestoTableColumn{
			Name: col.Name,
			Type: col.Type,
		})
	}
	for _, par := range params.Partitions {
		prestoTableCR.State.CreationParameters.Partitions = append(prestoTableCR.State.CreationParameters.Partitions, cbTypes.PrestoTableColumn{
			Name: par.Name,
			Type: par.Type,
		})
	}

	_, err := c.chargebackClient.ChargebackV1alpha1().PrestoTables(dataSource.Namespace).Create(&prestoTableCR)
	if err != nil {
		return err
	}
	return nil
}

func (c *Chargeback) updateDataSourceTableName(logger log.FieldLogger, dataSource *cbTypes.ReportDataSource, tableName string) error {
	dataSource.TableName = tableName
	_, err := c.chargebackClient.ChargebackV1alpha1().ReportDataSources(dataSource.Namespace).Update(dataSource)
	if err != nil {
		logger.WithError(err).Errorf("failed to update ReportDataSource table name for %q", dataSource.Name)
		return err
	}
	return nil
}