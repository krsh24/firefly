// Copyright © 2021 Kaleido, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package postgres

import (
	"github.com/kaleido-io/firefly/internal/config"
)

const (
	defaultMigrationsDirectory = "./db/migrations/postgres"
)

const (
	PSQLConfDatabase            = "database"
	PSQLConfURL                 = "url"
	PSQLConfMigrationsAuto      = "migrations.auto"
	PSQLConfMigrationsDirectory = "migrations.directory"
)

func (e *Postgres) InitPrefix(prefix config.Prefix) {
	prefix.AddKnownKey(PSQLConfDatabase)
	prefix.AddKnownKey(PSQLConfURL)
	prefix.AddKnownKey(PSQLConfMigrationsAuto)
	prefix.AddKnownKey(PSQLConfMigrationsDirectory, defaultMigrationsDirectory)
}
