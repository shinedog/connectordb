/**
Copyright (c) 2016 The ConnectorDB Contributors
Licensed under the MIT license.
**/
package restcore

import (
	"config"
	"connectordb/authoperator"
	"connectordb/datastream"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"server/webcore"
	"strconv"
	"strings"
	"sync/atomic"
	"util/datapoint"

	"github.com/gorilla/mux"
	"github.com/nu7hatch/gouuid"

	log "github.com/Sirupsen/logrus"
)

var (
	//ErrInvalidName is thrown when the name is bad
	ErrInvalidName = errors.New("The given name did not pass sanitation.")
	ErrBadQ        = errors.New("Unrecognized query command.")
	ErrCantParse   = errors.New("The given query cannot be parsed, since the values could not be extracted")
)

// safetyHeaders ensures that everything in the rest interface returns no-cache
func safetyHeaders(writer http.ResponseWriter) {
	writer.Header().Set("Cache-Control", "no-store, must-revalidate")
}

//OK is a simplifying function that returns success
func OK(writer http.ResponseWriter) error {
	writer.Header().Set("Content-Length", "2")
	safetyHeaders(writer)
	writer.WriteHeader(http.StatusOK)
	writer.Write([]byte("ok"))
	return nil
}

//JSONWriter writes the given data as http
func JSONWriter(writer http.ResponseWriter, data interface{}, logger *log.Entry, err error) (int, string) {
	if err != nil {
		return WriteError(writer, logger, http.StatusNotFound, err, false)
	}

	res, err := json.Marshal(data)
	if err != nil {
		return WriteError(writer, logger, http.StatusInternalServerError, err, true)

	}
	writer.Header().Set("Content-Length", strconv.Itoa(len(res)))
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	safetyHeaders(writer)
	writer.WriteHeader(http.StatusOK)
	writer.Write(res)
	return webcore.DEBUG, ""
}

//IntWriter writes an integer
func IntWriter(writer http.ResponseWriter, i int64, logger *log.Entry, err error) (int, string) {
	if err != nil {
		return WriteError(writer, logger, http.StatusForbidden, err, false)
	}

	res := []byte(strconv.FormatInt(i, 10))
	byteWriter(writer, res)
	return webcore.DEBUG, ""
}

//UintWriter writes an unsigned integer
func UintWriter(writer http.ResponseWriter, i uint64, logger *log.Entry, err error) (int, string) {
	if err != nil {
		return WriteError(writer, logger, http.StatusForbidden, err, false)
	}

	res := []byte(strconv.FormatUint(i, 10))
	byteWriter(writer, res)
	return webcore.DEBUG, ""
}

func byteWriter(writer http.ResponseWriter, b []byte) {
	writer.Header().Set("Content-Length", strconv.Itoa(len(b)))
	safetyHeaders(writer)
	writer.WriteHeader(http.StatusOK)
	writer.Write(b)
}

//UnmarshalRequest unmarshals the input data to the given interface
func UnmarshalRequest(request *http.Request, unmarshalTo interface{}) error {
	defer request.Body.Close()

	//Limit requests to the limit given in configuration
	data, err := ioutil.ReadAll(io.LimitReader(request.Body, config.Get().InsertLimitBytes))
	if err != nil {
		return err
	}

	return json.Unmarshal(data, unmarshalTo)
}

//ValidName sanitizes names so that only valid ones are added
func ValidName(n string, err error) error {
	if err != nil {
		return err
	}

	if strings.Contains(n, "/") ||
		strings.Contains(n, "\\") ||
		strings.Contains(n, " ") ||
		strings.Contains(n, "?") ||
		len(n) == 0 {
		return ErrInvalidName
	}

	return nil
}

//BadQ checks if there is a q= part to the given query, and gives an error if there is
func BadQ(o *authoperator.AuthOperator, writer http.ResponseWriter, request *http.Request, logger *log.Entry) error {
	if val := request.URL.Query().Get("q"); val != "" {
		return ErrBadQ
	}
	return nil
}

//ErrorResponse is the struct which holds the error message and response code
type ErrorResponse struct {
	Code      int    `json:"code"`
	Message   string `json:"msg"`
	Reference string `json:"ref,omitempty"`
}

//WriteError takes care of gracefully writing errors to the client in a way that allows
//for fairly easy debugging.
func WriteError(writer http.ResponseWriter, logger *log.Entry, errorCode int, err error, iserr bool) (int, string) {
	atomic.AddUint32(&webcore.StatsErrors, 1)

	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	safetyHeaders(writer)

	u, err2 := uuid.NewV4()
	if err2 != nil {
		logger.WithField("ref", "WEBERR").Errorln("Failed to generate error UUID: " + err2.Error())
		logger.WithField("ref", "WEBERR").Warningln("Original Error: " + err.Error())
		writer.WriteHeader(520)
		writer.Write([]byte(`{"code": 520, "msg": "Failed to generate error UUID", "ref": "WEBERR"}`))
		return webcore.INFO, ""
	}
	uu := u.String()

	response := ErrorResponse{
		Code:      errorCode,
		Message:   err.Error(),
		Reference: uu,
	}
	res, err2 := json.Marshal(response)
	if err2 != nil {
		logger.WithField("ref", uu).Errorln("Failed to marshal error struct: " + err2.Error())
		logger.WithField("ref", uu).Warningln("Original Error: " + err.Error())
		writer.WriteHeader(520)
		writer.Write([]byte(`{"code": 520, "msg": "Failed to write error message","ref":"` + uu + `"}`))
		return webcore.INFO, ""
	}

	//Now that we have the error message, we log it and send the messages
	l := logger.WithFields(log.Fields{"ref": uu, "code": errorCode})
	if iserr {
		l.Errorln(err.Error())
	} else {
		l.Warningln(err.Error())
	}
	writer.Header().Set("Content-Length", strconv.Itoa(len(res)))
	writer.WriteHeader(errorCode)
	writer.Write(res)
	return webcore.INFO, ""
}

//ParseIRange attempts to parse a request as an index range
func ParseIRange(q url.Values) (int64, int64, error) {
	i1s := q.Get("i1")
	i2s := q.Get("i2")
	if len(i1s) == 0 && len(i2s) == 0 {
		return 0, 0, ErrCantParse
	}
	i1, err := strconv.ParseInt(i1s, 0, 64)
	if i1s != "" && err != nil {
		return 0, 0, errors.New("Could not parse i1 parameter")
	}

	i2, err := strconv.ParseInt(i2s, 0, 64)
	if i2s != "" && err != nil {
		return 0, 0, errors.New("Could not parse i2 parameter")
	}

	return i1, i2, nil
}

//ParseTRange attempts to parse a request parameters as time range
func ParseTRange(q url.Values) (float64, float64, int64, error) {
	t1s := q.Get("t1")
	t2s := q.Get("t2")
	if len(t1s) == 0 && len(t2s) == 0 {
		return 0, 0, 0, ErrCantParse
	}
	t1, err := strconv.ParseFloat(t1s, 64)
	if t1s != "" && err != nil {
		return 0, 0, 0, errors.New("Could not parse t1 parameter")
	}

	t2, err := strconv.ParseFloat(t2s, 64)
	if t2s != "" && err != nil {
		return 0, 0, 0, errors.New("Could not parse t2 parameter")
	}

	lims := q.Get("limit")
	lim, err := strconv.ParseUint(lims, 0, 64)
	if lims != "" && err != nil {
		return 0, 0, 0, errors.New("Could not parse limit parameter.")
	}

	return t1, t2, int64(lim), nil
}

//WriteJSONResult writes a DataRange as a response
func WriteJSONResult(writer http.ResponseWriter, dr datastream.DataRange, logger *log.Entry, err error) (int, string) {
	if err != nil {
		return WriteError(writer, logger, http.StatusForbidden, err, false)
	}

	jreader, err := datapoint.NewJsonArrayReader(dr)
	if err != nil {
		if err == io.EOF {
			safetyHeaders(writer)
			writer.Header().Set("Content-Type", "application/json; charset=utf-8")
			writer.Header().Set("Content-Length", "2")
			writer.WriteHeader(http.StatusOK)
			writer.Write([]byte("[]")) //If there are no datapoints, just return empty
			return webcore.DEBUG, ""
		}
		return WriteError(writer, logger, http.StatusInternalServerError, err, true)
	}

	defer jreader.Close()
	safetyHeaders(writer)
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, err = io.Copy(writer, jreader)
	if err != nil {
		logger.Errorln(err)
		return 3, err.Error()
	}
	return webcore.DEBUG, ""
}

//GetStreamPath returns the relevant parts of a stream path
func GetStreamPath(request *http.Request) (username string, devicename string, streamname string, streampath string) {
	username = mux.Vars(request)["user"]
	devicename = mux.Vars(request)["device"]
	streamname = mux.Vars(request)["stream"]
	streampath = username + "/" + devicename + "/" + streamname

	// Get the downlink substream if it is queried
	v, err := strconv.ParseBool(request.URL.Query().Get("downlink"))
	if err == nil && v {
		streampath += "/downlink"
	}

	return username, devicename, streamname, streampath
}
