package migrations

import "github.com/BurntSushi/migration"

func AddTeamIDToBuilds(tx migration.LimitedTx) error {
	_, err := tx.Exec(`
		ALTER TABLE builds ADD COLUMN team_id integer REFERENCES teams (id);
	`)
	if err != nil {
		return err
	}

	_, err = tx.Exec(`
		UPDATE builds
		SET team_id = (
			SELECT id
			FROM teams
			WHERE name = 'main'
		)
	`)
	return err
}
