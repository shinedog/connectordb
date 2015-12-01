package server

import (
	"config"
	"connectordb"
	"errors"
	"fmt"
	"net/http"
	"server/restapi"
	"server/restapi/restcore"
	"server/webcore"
	"server/website"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/gorilla/mux"

	log "github.com/Sirupsen/logrus"
)

var (
	//PreferredFileLimit sets the preferred maximum number of open files
	PreferredFileLimit = uint64(10000)
)

//SetFileLimit attempts to set the open file limits
func SetFileLimit() {
	var noFile syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &noFile)
	if err != nil {
		log.Warn("Could not read file limit:", err)
		return
	}
	if noFile.Cur < PreferredFileLimit {
		change := uint64(0)
		if noFile.Max < PreferredFileLimit {
			change = noFile.Max
			log.Warnf("User hard file limit (%d) is less than preferred %d", noFile.Max, PreferredFileLimit)
		} else {
			change = PreferredFileLimit
		}
		log.Warnf("Setting user file limit from %d to %d", noFile.Cur, change)
		noFile.Cur = change
		if err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &noFile); err != nil {
			log.Error("Failed to set file limit: ", err)
		}
	}
}

//SecurityHeaderHandler provides a wrapper function for an http.Handler that sets several security headers for all sessions passing through
func SecurityHeaderHandler(h http.Handler) http.Handler {

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {

		// See the OWASP security project for these headers:
		// https://www.owasp.org/index.php/List_of_useful_HTTP_headers

		// Don't allow our site to be embedded in another
		writer.Header().Set("X-Frame-Options", "deny")

		// Enable the client side XSS filter
		writer.Header().Set("X-XSS-Protection", "1; mode=block")

		// Disable content sniffing which could lead to improperly executed
		// scripts or such from malicious user uploads
		writer.Header().Set("X-Content-Type-Options", "nosniff")

		h.ServeHTTP(writer, request)
	})
}

//OptionsHandler on OPTIONS to allow cross-site XMLHTTPRequest, allow access control origin
func OptionsHandler(writer http.ResponseWriter, request *http.Request) {
	webcore.GetRequestLogger(request, "OPTIONS").Debug()
	webcore.WriteAccessControlHeaders(writer, request)

	//These headers are only needed for the OPTIONS request
	writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE")
	writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	writer.WriteHeader(http.StatusOK)
}

// NotFoundHandler handles 404 errors for the whole server
func NotFoundHandler(writer http.ResponseWriter, request *http.Request) {
	if strings.HasPrefix(request.URL.Path, "/api") {
		logger := webcore.GetRequestLogger(request, "404")
		//If this is a REST API call, write a REST-like error
		atomic.AddUint32(&webcore.StatsRESTQueries, 1)
		restcore.WriteError(writer, logger, http.StatusNotFound, errors.New("This path is not recognized"), false)
		return
	}

	//Otherwise, we assume that it is the web not found handler
	website.NotFoundHandler(writer, request)

}

//RunServer runs the ConnectorDB frontend server
func RunServer(c *config.Configuration) error {
	SetFileLimit()
	err := webcore.Initialize(c)
	if err != nil {
		return err
	}

	//Connect using the configuration
	db, err := connectordb.Open(c.Options())
	if err != nil {
		return err
	}

	r := mux.NewRouter()

	//Allow for the application to match /path and /path/ to the same place.
	r.StrictSlash(true)

	//Setup the 404 handler
	r.NotFoundHandler = http.HandlerFunc(NotFoundHandler)

	r.Methods("OPTIONS").Handler(http.HandlerFunc(OptionsHandler))

	//The rest api has its own versioned url
	s := r.PathPrefix("/api/v1").Subrouter()
	_, err = restapi.Router(db, s)
	if err != nil {
		return err
	}

	//The website is initialized at /
	_, err = website.Router(db, r)
	if err != nil {
		return err
	}

	//Set up the web server
	http.Handle("/", SecurityHeaderHandler(r))

	//Show the statistics
	go webcore.RunStats()
	go webcore.RunQueryTimers()

	//Run the dbwriter
	go db.RunWriter()

	log.Infof("Running ConnectorDB v%s at %s", connectordb.Version, c.SiteName)

	return http.ListenAndServe(fmt.Sprintf("%s:%d", c.Hostname, c.Port), nil)
}