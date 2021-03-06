package commands

import (
	"config"
	"connectordb"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"util"

	"connectordb/datastream"
	"connectordb/users"

	log "github.com/Sirupsen/logrus"
	"github.com/spf13/cobra"
)

type importContext struct {
	ExportInfo
	db *connectordb.Database
}

//DatapointReader
type DatapointReader struct {
	dec *json.Decoder
}

// Next allows us to conform to the DatapointIterator interface
func (r *DatapointReader) Next() (*datastream.Datapoint, error) {
	if r.dec.More() {
		// There is another datapoint
		dp := &datastream.Datapoint{}
		err := r.dec.Decode(dp)
		return dp, err
	}
	return nil, nil
}

// Returns a datapoint reader that reads the json from file
// Copied from pipescript's code
func NewDatapointReader(r io.Reader) (*DatapointReader, error) {
	dec := json.NewDecoder(r)
	_, err := dec.Token() // Read starting value
	if err != nil {
		return nil, err
	}
	return &DatapointReader{dec}, nil
}

// Given a filename, imports a stream's data from the file
func importStreamData(c *importContext, dbpath string, streamID int64, substream string, filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	dr, err := NewDatapointReader(f)
	if err != nil {
		return err
	}

	var dpa []datastream.Datapoint
	size := 0
	totalpoints := 0
	dp, err := dr.Next()
	for err == nil && dp != nil {
		totalpoints += 1
		dpa = append(dpa, *dp)

		// We want to make sure that we insert less than 10MB at a time
		dpb, err := dp.Bytes()
		if err != nil {
			return err
		}
		size += len(dpb)

		if size > 10000000 || len(dpa) > 10000 {
			// We insert the data batch
			if err = c.db.InsertStreamByID(streamID, substream, dpa, false); err != nil {
				return err
			}
			size = 0
			dpa = nil
			if substream == "" {
				log.Debug("................ ", totalpoints, " points inserted")
			} else {
				log.Debug("................ ", totalpoints, " points inserted to ", substream)
			}

		}
		dp, err = dr.Next()

	}
	if err != nil {
		return err
	}
	if len(dpa) > 0 {
		if err = c.db.InsertStreamByID(streamID, substream, dpa, false); err != nil {
			return err
		}
		if substream == "" {
			log.Debug("................ ", totalpoints, " points inserted")
		} else {
			log.Debug("................ ", totalpoints, " points inserted to ", substream)
		}
	}

	return nil
}

// importStream imports the stream from the given directory, given a deviceID
// and a directory where the stream resides
func importStream(c *importContext, dbpath string, deviceID int64, dir string) error {
	// First make sure that the necessary files exist

	b, err := ioutil.ReadFile(path.Join(dir, "stream.json"))
	if err != nil {
		return err
	}
	var sm users.StreamMaker

	if err = json.Unmarshal(b, &sm); err != nil {
		return err
	}
	sm.DeviceID = deviceID

	log.Debug("............. ", dbpath)

	// We need to temporarily disable the stream schema, so that we can insert data even if
	// the schema was changed earlier by the user, so that older datapoints are incompatible
	schema := sm.Schema
	sm.Schema = "{}"

	// Create the stream
	if err = c.db.CreateStreamByDeviceID(&sm); err != nil {
		return err
	}

	// Get the streamID
	s, err := c.db.ReadStreamByDeviceID(deviceID, sm.Name)
	if err != nil {
		return err
	}

	// Now import the data from file
	if err = importStreamData(c, dbpath, s.StreamID, "", path.Join(dir, "data.json")); err != nil {
		return err
	}

	// If the stream is a downlink, import the downlink also
	if s.Downlink {
		if err = importStreamData(c, dbpath, s.StreamID, "downlink", path.Join(dir, "downlink.json")); err != nil {
			return err
		}
	}

	// Now finally set the stream schema again if it isn't {}
	if schema != "{}" {
		if err = c.db.UpdateStreamByID(s.StreamID, map[string]interface{}{"schema": schema}); err != nil {
			return err
		}
	}

	return nil
}

func importDevice(c *importContext, dbpath string, userID int64, dir string) error {
	b, err := ioutil.ReadFile(path.Join(dir, "device.json"))
	if err != nil {
		return err
	}
	var dm users.DeviceMaker

	if err = json.Unmarshal(b, &dm); err != nil {
		return err
	}
	dm.UserID = userID

	log.Info("............. ", dbpath)

	// Now, we create the device, with a couple caveats: If it is a user device, it already exists,
	// since it is created with the user. We simply update the device. And the meta device is ignored entirely
	if dm.Name == "meta" {
		return nil
	}

	if dm.Name == "user" {
		// The user device. We get the existing user device
		d, err := c.db.ReadDeviceByUserID(userID, "user")
		if err != nil {
			return err
		}
		// Set the ID
		dm.DeviceID = d.DeviceID
		if err = c.db.Userdb.UpdateDevice(&dm.Device); err != nil {
			return err
		}
	} else {
		if err = c.db.CreateDeviceByUserID(&dm); err != nil {
			return err
		}
	}
	d, err := c.db.ReadDeviceByUserID(userID, dm.Name)
	if err != nil {
		return err
	}

	// Now go through all of the device's streams and import those too
	dirlist, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}

	for i := range dirlist {
		if dirlist[i].IsDir() {
			spath := dbpath + "/" + dirlist[i].Name()
			if err = importStream(c, spath, d.DeviceID, path.Join(dir, dirlist[i].Name())); err != nil {
				return err
			}
		}
	}

	return nil
}

func importUser(c *importContext, dir string) error {
	b, err := ioutil.ReadFile(path.Join(dir, "user.json"))
	if err != nil {
		return err
	}
	var um users.UserMaker

	if err = json.Unmarshal(b, &um); err != nil {
		return err
	}

	log.Info("... Importing ", um.Name)

	// For version 1 of import, we set the password to the user name.
	// In the UserMaker, hash scheme and other stuff is ignored
	um.Password = um.Name

	if err = c.db.CreateUser(&um); err != nil {
		return err
	}

	u, err := c.db.ReadUser(um.Name)
	if err != nil {
		return err
	}

	// If the import is version 2, we now manually update the password
	// to reflect the old password
	if c.Version == 2 {
		if err = json.Unmarshal(b, &u); err != nil {
			return err
		}
		if err = c.db.Userdb.UpdateUser(u); err != nil {
			return err
		}
	} else {
		log.Warn("Unable to recover password for ", u.Name, ". Setting password=username.")
	}

	// And now import all of the user's devices
	// Now go through all of the device's streams and import those too
	dirlist, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}

	for i := range dirlist {
		if dirlist[i].IsDir() {
			dpath := u.Name + "/" + dirlist[i].Name()
			if err = importDevice(c, dpath, u.UserID, path.Join(dir, dirlist[i].Name())); err != nil {
				return err
			}
		}
	}

	return nil

}

// ImportCmd imports a data dump
var ImportCmd = &cobra.Command{
	Use:   "import [config file path or database directory] [export directory]",
	Short: "Imports an exported ConnectorDB database",
	Long: `Allows populating an empty ConnectorDB database with data from
another ConnectorDB instance, or a previous version of ConnectorDB.
It is given the directory where a ConnectorDB export was performed.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return ErrConfig
		}

		if len(args) < 2 {
			return errors.New("Must specify a directory from which to import")
		}
		if len(args) > 2 {
			return ErrTooManyArgs
		}

		cfg, err := config.LoadConfig(args[0])
		if err != nil {
			return err
		}

		setLogging(cfg)

		// Now see if the folder exists
		dir, err := filepath.Abs(args[1])
		if err != nil {
			return err
		}
		if !util.PathExists(dir) {
			return errors.New("Could not find the folder to import")
		}

		// Now read the export file
		b, err := ioutil.ReadFile(path.Join(dir, "connectordb.json"))
		if err != nil {
			return err
		}

		var info importContext
		if err = json.Unmarshal(b, &info); err != nil {
			return err
		}
		if info.Version <= 0 || info.Version > 2 {
			return errors.New("Can't open the export version")
		}

		// Open the ConnectorDB database
		db, err := connectordb.Open(cfg.Options())
		if err != nil {
			return err
		}
		defer db.Close()

		//Set up the database in context
		info.db = db

		log.Info("Import format version ", info.Version, ", from ConnectorDB v", info.ConnectorDB)

		// List all users in the export
		dread, err := ioutil.ReadDir(dir)
		if err != nil {
			return err
		}
		for i := range dread {
			if dread[i].IsDir() {
				udir := path.Join(dir, dread[i].Name())

				if err = importUser(&info, udir); err != nil {
					return err
				}
			}
		}

		return nil
	},
}

func init() {
	RootCmd.AddCommand(ImportCmd)
}
