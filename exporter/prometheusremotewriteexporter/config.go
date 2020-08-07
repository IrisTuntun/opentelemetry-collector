// Copyright 2020 The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This package defines the standard and necessary parameters of the exporter config struct.
// The yaml file for the entire collector pipelne must include a section underneath
// `exporters` titled `prometheusremotewrite(/#)`. Example in testdata/config.yaml.

package prometheusremotewriteexporter

import (
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configmodels"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
)

// Config defines configuration for Remote Write exporter.
type Config struct {
	// squash ensures fields are correctly decoded in embedded struct.
	configmodels.ExporterSettings  `mapstructure:",squash"`
	exporterhelper.TimeoutSettings `mapstructure:",squash"`

	exporterhelper.QueueSettings `mapstructure:"sending_queue"`
	exporterhelper.RetrySettings `mapstructure:"retry_on_failure"`

	// Namespace if set, exports metrics under the provided value.*/
	Namespace string `mapstructure:"namespace"`

	// Optional headers configuration for authorization and security/extra metadata
	Headers map[string]string `mapstructure:"headers"`

	HTTPClientSettings confighttp.HTTPClientSettings `mapstructure:"http_setting"`
}
