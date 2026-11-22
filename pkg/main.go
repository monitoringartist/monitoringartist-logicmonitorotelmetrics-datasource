package main

import (
	"os"

	"github.com/grafana/grafana-plugin-sdk-go/backend/datasource"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	promds "github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/datasource"
)

func main() {
	if err := datasource.Manage("monitoringartist-logicmonitorotelmetrics-datasource", promds.NewDatasource, datasource.ManageOpts{}); err != nil {
		log.DefaultLogger.Error(err.Error())
		os.Exit(1)
	}
}
