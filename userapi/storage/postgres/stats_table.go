// Copyright 2022 The Matrix.org Foundation C.I.C.
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
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"
	"github.com/matrix-org/dendrite/internal"
	"github.com/matrix-org/dendrite/internal/sqlutil"
	"github.com/matrix-org/dendrite/userapi/api"
	"github.com/matrix-org/dendrite/userapi/storage/tables"
	"github.com/matrix-org/dendrite/userapi/types"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/sirupsen/logrus"
)

const userDailyVisitsSchema = `
CREATE TABLE IF NOT EXISTS user_daily_visits (
    localpart TEXT NOT NULL,
	device_id TEXT NOT NULL,
	timestamp BIGINT NOT NULL,
	user_agent TEXT
);

-- Device IDs and timestamp must be unique for a given user per day
CREATE UNIQUE INDEX IF NOT EXISTS localpart_device_timestamp_idx ON user_daily_visits(localpart, device_id, timestamp);
CREATE INDEX IF NOT EXISTS timestamp_idx ON user_daily_visits(timestamp);
CREATE INDEX IF NOT EXISTS localpart_timestamp_idx ON user_daily_visits(localpart, timestamp);
`

const countUsersLastSeenAfterSQL = "" +
	"SELECT COUNT(*) FROM (" +
	" SELECT localpart FROM device_devices WHERE last_seen_ts > $1 " +
	" GROUP BY localpart" +
	" ) u"

/*
R30Users counts the number of 30 day retained users, defined as:
- Users who have created their accounts more than 30 days ago
- Where last seen at most 30 days ago
- Where account creation and last_seen are > 30 days apart
*/
const countR30UsersSQL = `
SELECT platform, COUNT(*) FROM (
	SELECT users.localpart, platform, users.created_ts, MAX(uip.last_seen_ts)
	FROM account_accounts users
	INNER JOIN
	(SELECT 
		localpart, last_seen_ts,
		CASE
	    	WHEN user_agent LIKE '%%Android%%' THEN 'android'
    	    WHEN user_agent LIKE '%%iOS%%' THEN 'ios'
        	WHEN user_agent LIKE '%%Electron%%' THEN 'electron'
        	WHEN user_agent LIKE '%%Mozilla%%' THEN 'web'
        	WHEN user_agent LIKE '%%Gecko%%' THEN 'web'
        	ELSE 'unknown'
		END
    	AS platform
		FROM device_devices
	) uip
	ON users.localpart = uip.localpart
	AND users.account_type <> 4
	AND users.created_ts < $1
	AND uip.last_seen_ts > $1
	AND (uip.last_seen_ts) - users.created_ts > $2
	GROUP BY users.localpart, platform, users.created_ts
	) u GROUP BY PLATFORM
`

/*
R30UsersV2 counts the number of 30 day retained users, defined as users that:
- Appear more than once in the past 60 days
- Have more than 30 days between the most and least recent appearances that occurred in the past 60 days.
*/
const countR30UsersV2SQL = `
SELECT
	client_type,
    count(client_type)
FROM 
	(
    	SELECT
        	localpart,
            CASE
            	WHEN
                LOWER(user_agent) LIKE '%%riot%%' OR
				LOWER(user_agent) LIKE '%%element%%'
                THEN CASE
                	WHEN LOWER(user_agent) LIKE '%%electron%%' THEN 'electron'
					WHEN LOWER(user_agent) LIKE '%%android%%' THEN 'android'
					WHEN LOWER(user_agent) LIKE '%%ios%%' THEN 'ios'
					ELSE 'unknown'
				END
				WHEN LOWER(user_agent) LIKE '%%mozilla%%' OR LOWER(user_agent) LIKE '%%gecko%%' THEN 'web'
				ELSE 'unknown'
			END as client_type
		FROM user_daily_visits
		WHERE timestamp > $1 AND timestamp < $2
		GROUP BY localpart, client_type
		HAVING max(timestamp) - min(timestamp) > $3
	) AS temp
GROUP BY client_type
`

const countUserByAccountTypeSQL = `
SELECT COUNT(*) FROM account_accounts WHERE account_type = ANY($1)
`

// $1 = All non guest AccountType IDs
// $2 = Guest AccountType
const countRegisteredUserByTypeStmt = `
SELECT user_type, COUNT(*) AS count FROM (
	SELECT
    CASE
    	WHEN account_type = ANY($1) AND appservice_id IS NULL THEN 'native'
        WHEN account_type = $2 AND appservice_id IS NULL THEN 'guest'
        WHEN account_type = ANY($1) AND appservice_id IS NOT NULL THEN 'bridged'
	END AS user_type
    FROM account_accounts
    WHERE created_ts > $3
) AS t GROUP BY user_type
`

// account_type 1 = users; 3 = admins
const updateUserDailyVisitsSQL = `
INSERT INTO user_daily_visits(localpart, device_id, timestamp, user_agent)
	SELECT u.localpart, u.device_id, $1, MAX(u.user_agent)
	FROM device_devices AS u
	LEFT JOIN (
		SELECT localpart, device_id, timestamp FROM user_daily_visits
		WHERE timestamp = $1
	) udv
	ON u.localpart = udv.localpart AND u.device_id = udv.device_id
	INNER JOIN device_devices d ON d.localpart = u.localpart
	INNER JOIN account_accounts a ON a.localpart = u.localpart
	WHERE $2 <= d.last_seen_ts AND d.last_seen_ts < $3
	AND a.account_type in (1, 3)
	GROUP BY u.localpart, u.device_id
ON CONFLICT (localpart, device_id, timestamp) DO NOTHING
;
`

const queryDBEngineVersion = "SHOW server_version;"

type statsStatements struct {
	serverName                    gomatrixserverlib.ServerName
	lastUpdate                    time.Time
	countUsersLastSeenAfterStmt   *sql.Stmt
	countR30UsersStmt             *sql.Stmt
	countR30UsersV2Stmt           *sql.Stmt
	updateUserDailyVisitsStmt     *sql.Stmt
	countUserByAccountTypeStmt    *sql.Stmt
	countRegisteredUserByTypeStmt *sql.Stmt
	dbEngineVersionStmt           *sql.Stmt
}

func NewPostgresStatsTable(db *sql.DB, serverName gomatrixserverlib.ServerName) (tables.StatsTable, error) {
	s := &statsStatements{
		serverName: serverName,
		lastUpdate: time.Now(),
	}

	_, err := db.Exec(userDailyVisitsSchema)
	if err != nil {
		return nil, err
	}
	go s.startTimers()
	return s, sqlutil.StatementList{
		{&s.countUsersLastSeenAfterStmt, countUsersLastSeenAfterSQL},
		{&s.countR30UsersStmt, countR30UsersSQL},
		{&s.countR30UsersV2Stmt, countR30UsersV2SQL},
		{&s.updateUserDailyVisitsStmt, updateUserDailyVisitsSQL},
		{&s.countUserByAccountTypeStmt, countUserByAccountTypeSQL},
		{&s.countRegisteredUserByTypeStmt, countRegisteredUserByTypeStmt},
		{&s.dbEngineVersionStmt, queryDBEngineVersion},
	}.Prepare(db)
}

func (s *statsStatements) startTimers() {
	var updateStatsFunc func()
	updateStatsFunc = func() {
		logrus.Infof("Executing UpdateUserDailyVisits")
		if err := s.updateUserDailyVisits(context.Background(), nil); err != nil {
			logrus.WithError(err).Error("failed to update daily user visits")
		}
		time.AfterFunc(time.Hour*3, updateStatsFunc)
	}
	time.AfterFunc(time.Minute*5, updateStatsFunc)
}

func (s *statsStatements) allUsers(ctx context.Context, txn *sql.Tx) (result int64, err error) {
	stmt := sqlutil.TxStmt(txn, s.countUserByAccountTypeStmt)
	err = stmt.QueryRowContext(ctx,
		pq.Int64Array{
			int64(api.AccountTypeUser),
			int64(api.AccountTypeGuest),
			int64(api.AccountTypeAdmin),
			int64(api.AccountTypeAppService),
		},
	).Scan(&result)
	return
}

func (s *statsStatements) nonBridgedUsers(ctx context.Context, txn *sql.Tx) (result int64, err error) {
	stmt := sqlutil.TxStmt(txn, s.countUserByAccountTypeStmt)
	err = stmt.QueryRowContext(ctx,
		pq.Int64Array{
			int64(api.AccountTypeUser),
			int64(api.AccountTypeGuest),
			int64(api.AccountTypeAdmin),
		},
	).Scan(&result)
	return
}

func (s *statsStatements) registeredUserByType(ctx context.Context, txn *sql.Tx) (map[string]int64, error) {
	stmt := sqlutil.TxStmt(txn, s.countRegisteredUserByTypeStmt)
	registeredAfter := time.Now().AddDate(0, 0, -1)

	rows, err := stmt.QueryContext(ctx,
		pq.Int64Array{
			int64(api.AccountTypeUser),
			int64(api.AccountTypeAdmin),
			int64(api.AccountTypeAppService),
		},
		api.AccountTypeGuest,
		gomatrixserverlib.AsTimestamp(registeredAfter),
	)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "RegisteredUserByType: failed to close rows")

	var userType string
	var count int64
	var result = make(map[string]int64)
	for rows.Next() {
		if err = rows.Scan(&userType, &count); err != nil {
			return nil, err
		}
		result[userType] = count
	}

	return result, rows.Err()
}

func (s *statsStatements) dailyUsers(ctx context.Context, txn *sql.Tx) (result int64, err error) {
	stmt := sqlutil.TxStmt(txn, s.countUsersLastSeenAfterStmt)
	lastSeenAfter := time.Now().AddDate(0, 0, -1)
	err = stmt.QueryRowContext(ctx,
		gomatrixserverlib.AsTimestamp(lastSeenAfter),
	).Scan(&result)
	return
}

func (s *statsStatements) monthlyUsers(ctx context.Context, txn *sql.Tx) (result int64, err error) {
	stmt := sqlutil.TxStmt(txn, s.countUsersLastSeenAfterStmt)
	lastSeenAfter := time.Now().AddDate(0, 0, -30)
	err = stmt.QueryRowContext(ctx,
		gomatrixserverlib.AsTimestamp(lastSeenAfter),
	).Scan(&result)
	return
}

/*
R30Users counts the number of 30 day retained users, defined as:
- Users who have created their accounts more than 30 days ago
- Where last seen at most 30 days ago
- Where account creation and last_seen are > 30 days apart
*/
func (s *statsStatements) r30Users(ctx context.Context, txn *sql.Tx) (map[string]int64, error) {
	stmt := sqlutil.TxStmt(txn, s.countR30UsersStmt)
	lastSeenAfter := time.Now().AddDate(0, 0, -30)
	diff := time.Hour * 24 * 30

	rows, err := stmt.QueryContext(ctx,
		gomatrixserverlib.AsTimestamp(lastSeenAfter),
		diff.Milliseconds(),
	)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "R30Users: failed to close rows")

	var platform string
	var count int64
	var result = make(map[string]int64)
	for rows.Next() {
		if err = rows.Scan(&platform, &count); err != nil {
			return nil, err
		}
		result["all"] += count
		if platform == "unknown" {
			continue
		}
		result[platform] = count
	}

	return result, rows.Err()
}

/*
R30UsersV2 counts the number of 30 day retained users, defined as users that:
- Appear more than once in the past 60 days
- Have more than 30 days between the most and least recent appearances that occurred in the past 60 days.
*/
func (s *statsStatements) r30UsersV2(ctx context.Context, txn *sql.Tx) (map[string]int64, error) {
	stmt := sqlutil.TxStmt(txn, s.countR30UsersV2Stmt)
	sixtyDaysAgo := time.Now().AddDate(0, 0, -60)
	thirtyDaysAgo := time.Now().AddDate(0, 0, -30)
	tomorrow := time.Now().Add(time.Hour * 24)

	rows, err := stmt.QueryContext(ctx,
		gomatrixserverlib.AsTimestamp(sixtyDaysAgo),
		gomatrixserverlib.AsTimestamp(tomorrow),
		gomatrixserverlib.AsTimestamp(thirtyDaysAgo),
	)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "R30UsersV2: failed to close rows")

	var platform string
	var count int64
	var result = map[string]int64{
		"ios":      0,
		"android":  0,
		"web":      0,
		"electron": 0,
		"all":      0,
	}
	for rows.Next() {
		if err = rows.Scan(&platform, &count); err != nil {
			return nil, err
		}
		result["all"] += count
		if platform == "unknown" {
			continue
		}
		result[platform] = count
	}

	return result, rows.Err()
}

// UserStatistics collects some information about users on this instance.
// Returns the stats itself as well as the database engine version and type.
// On error, returns the stats collected up to the error.
func (s *statsStatements) UserStatistics(ctx context.Context, txn *sql.Tx) (*types.UserStatistics, *types.DatabaseEngine, error) {
	var (
		stats = &types.UserStatistics{
			R30UsersV2: map[string]int64{
				"ios":      0,
				"android":  0,
				"web":      0,
				"electron": 0,
				"all":      0,
			},
			R30Users:              map[string]int64{},
			RegisteredUsersByType: map[string]int64{},
		}
		dbEngine = &types.DatabaseEngine{Engine: "Postgres", Version: "unknown"}
		err      error
	)
	stats.AllUsers, err = s.allUsers(ctx, txn)
	if err != nil {
		return stats, dbEngine, err
	}
	stats.DailyUsers, err = s.dailyUsers(ctx, txn)
	if err != nil {
		return stats, dbEngine, err
	}
	stats.MonthlyUsers, err = s.monthlyUsers(ctx, txn)
	if err != nil {
		return stats, dbEngine, err
	}
	stats.R30Users, err = s.r30Users(ctx, txn)
	if err != nil {
		return stats, dbEngine, err
	}
	stats.R30UsersV2, err = s.r30UsersV2(ctx, txn)
	if err != nil {
		return stats, dbEngine, err
	}
	stats.NonBridgedUsers, err = s.nonBridgedUsers(ctx, txn)
	if err != nil {
		return stats, dbEngine, err
	}
	stats.RegisteredUsersByType, err = s.registeredUserByType(ctx, txn)
	if err != nil {
		return stats, dbEngine, err
	}

	stmt := sqlutil.TxStmt(txn, s.dbEngineVersionStmt)
	err = stmt.QueryRowContext(ctx).Scan(&dbEngine.Version)
	return stats, dbEngine, err
}

func (s *statsStatements) updateUserDailyVisits(ctx context.Context, txn *sql.Tx) error {
	stmt := sqlutil.TxStmt(txn, s.updateUserDailyVisitsStmt)
	_ = stmt
	todayStart := time.Now().Truncate(time.Hour * 24)

	// edge case
	if todayStart.After(s.lastUpdate) {
		todayStart = todayStart.AddDate(0, 0, -1)
	}
	_, err := stmt.ExecContext(ctx,
		gomatrixserverlib.AsTimestamp(todayStart),
		gomatrixserverlib.AsTimestamp(s.lastUpdate),
		gomatrixserverlib.AsTimestamp(time.Now()),
	)
	if err == nil {
		s.lastUpdate = time.Now()
	}
	return err
}
