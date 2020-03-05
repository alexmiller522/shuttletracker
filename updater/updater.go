package updater

import (
	"io/ioutil"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/viper"

	"github.com/wtg/shuttletracker"
	"github.com/wtg/shuttletracker/log"
	"github.com/wtg/shuttletracker/smooth"
)

// Updater handles periodically grabbing the latest vehicle location data from iTrak.
type Updater struct {
	cfg                  Config
	updateInterval       time.Duration
	dataRegexp           *regexp.Regexp
	ms                   shuttletracker.ModelService
	mutex                *sync.Mutex
	lastDataFeedResponse *shuttletracker.DataFeedResponse
	sm                   *sync.Mutex
	vehicleIDs           []int64
	subscribers          []func(*shuttletracker.Location)
	predictions          map[int64]smooth.Prediction       // DEBUG: Track recent positions
	locations            map[int64]shuttletracker.Location // Tracks the last real update each vehicle received
}

type Config struct {
	DataFeed       string
	UpdateInterval string
}

// New creates an Updater.
func New(cfg Config, ms shuttletracker.ModelService) (*Updater, error) {
	updater := &Updater{
		cfg:         cfg,
		ms:          ms,
		mutex:       &sync.Mutex{},
		sm:          &sync.Mutex{},
		subscribers: []func(*shuttletracker.Location){},
		predictions: make(map[int64]smooth.Prediction),
		locations:   make(map[int64]shuttletracker.Location),
	}

	interval, err := time.ParseDuration(cfg.UpdateInterval)
	if err != nil {
		return nil, err
	}
	updater.updateInterval = interval

	// Match each API field with any number (+)
	//   of the previous expressions (\d digit, \. escaped period, - negative number)
	//   Specify named capturing groups to store each field from data feed
	updater.dataRegexp = regexp.MustCompile(`(?P<id>Vehicle ID:([\d\.]+)) (?P<lat>lat:([\d\.-]+)) (?P<lng>lon:([\d\.-]+)) (?P<heading>dir:([\d\.-]+)) (?P<speed>spd:([\d\.-]+)) (?P<lock>lck:([\d\.-]+)) (?P<time>time:([\d]+)) (?P<date>date:([\d]+)) (?P<status>trig:([\d]+))`)

	return updater, nil
}

func NewConfig(v *viper.Viper) *Config {
	cfg := &Config{
		UpdateInterval: "10s",
		DataFeed:       "https://shuttles.rpi.edu/datafeed",
	}
	v.SetDefault("updater.updateinterval", cfg.UpdateInterval)
	v.SetDefault("updater.datafeed", cfg.DataFeed)
	return cfg
}

// Run updater forever.
func (u *Updater) Run() {
	log.Debug("Updater started.")
	ticker := time.Tick(time.Second)

	// Do one initial update.
	u.update()

	// Call predict(id) every 1s and update() every 10s
	intervalTracker := 1
	for range ticker {
		if intervalTracker%10 == 0 {
			u.update()
			intervalTracker = 0
		} else {
			for _, id := range u.vehicleIDs {
				u.predict(id)
			}
		}
		intervalTracker++
	}
}

// Subscribe allows callers to provide a function that is called after Updater parses a new Location.
func (u *Updater) Subscribe(f func(*shuttletracker.Location)) {
	u.sm.Lock()
	u.subscribers = append(u.subscribers, f)
	u.sm.Unlock()
}

func (u *Updater) notifySubscribers(loc *shuttletracker.Location) {
	u.sm.Lock()
	for _, sub := range u.subscribers {
		go sub(loc)
	}
	u.sm.Unlock()
}

// Makes a prediction on this vehicle's current location, stores it in the updater's predictions map,
// and updates the vehicle's marker on the map. Assumes the vehicle is on a valid route.
func (u *Updater) predict(vehicleID int64) {
	vehicle, err := u.ms.Vehicle(vehicleID)
	if err != nil {
		log.WithError(err).Error("unable to get vehicle from ID")
		return
	}
	if update, exists := u.locations[vehicle.ID]; exists {
		route, err := u.ms.Route(*update.RouteID)
		if err != nil {
			log.WithError(err).Error("unable to get route for prediction")
		}
		if route != nil {
			prediction := smooth.NaivePredictPosition(vehicle, &update, route)
			newLocation := prediction.Point
			newUpdate := &shuttletracker.Location{
				TrackerID: update.TrackerID,
				Latitude:  newLocation.Latitude,
				Longitude: newLocation.Longitude,
				Heading:   update.Heading,
				Speed:     update.Speed,
				Time:      time.Now(),
				RouteID:   &route.ID,
			}
			u.predictions[vehicle.ID] = prediction // DEBUG
			if err := u.ms.CreateLocation(newUpdate); err != nil {
				log.WithError(err).Error("could not create location for prediction")
			}
			//u.notifySubscribers(newUpdate)
		}
	} else {
		log.Debugf("no updates to make a prediction from")
	}
}

// Send a request to iTrak API, get updated shuttle info,
// store updated records in the database, and remove old records.
func (u *Updater) update() {
	// Make request to iTrak data feed
	client := http.Client{Timeout: time.Second * 5}
	resp, err := client.Get(u.cfg.DataFeed)
	if err != nil {
		log.WithError(err).Error("Could not get data feed.")
		return
	}

	if resp.StatusCode != http.StatusOK {
		log.Errorf("data feed status code %d", resp.StatusCode)
		return
	}

	// Read response body content
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.WithError(err).Error("Could not read data feed.")
		return
	}
	resp.Body.Close()

	dfresp := &shuttletracker.DataFeedResponse{
		Body:       body,
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
	}
	u.setLastResponse(dfresp)

	delim := "eof"
	// split the body of response by delimiter
	vehiclesData := strings.Split(string(body), delim)
	vehiclesData = vehiclesData[:len(vehiclesData)-1] // last element is EOF

	// TODO: Figure out if this handles == 1 vehicle correctly or always assumes > 1.
	if len(vehiclesData) <= 1 {
		log.Warnf("Found no vehicles delineated by '%s'.", delim)
	}

	// Clear vehicle IDs before we add new ones
	//u.vehicleIDs = u.vehicleIDs[:0]

	wg := sync.WaitGroup{}
	// for parsed data, update each vehicle
	for _, vehicleData := range vehiclesData {
		wg.Add(1)
		go func(vehicleData string) {
			u.handleVehicleData(vehicleData)
			wg.Done()
		}(vehicleData)
	}
	wg.Wait()
	log.Debugf("Updated vehicles.")

	// Prune updates older than one month
	deleted, err := u.ms.DeleteLocationsBefore(time.Now().AddDate(0, -1, 0))
	if err != nil {
		log.WithError(err).Error("unable to remove old locations")
		return
	}
	if deleted > 0 {
		log.Debugf("Removed %d old updates.", deleted)
	}
}

// nolint: gocyclo
func (u *Updater) handleVehicleData(vehicleData string) {
	match := u.dataRegexp.FindAllStringSubmatch(vehicleData, -1)[0]
	// Store named capturing group and matching expression as a key value pair
	result := map[string]string{}
	for i, item := range match {
		result[u.dataRegexp.SubexpNames()[i]] = item
	}

	// Create new vehicle update & insert update into database

	itrakID := strings.Replace(result["id"], "Vehicle ID:", "", -1)
	vehicle, err := u.ms.VehicleWithTrackerID(itrakID)
	if err == shuttletracker.ErrVehicleNotFound {
		log.Warnf("Unknown vehicle ID \"%s\" returned by iTrak. Make sure all vehicles have been added.", itrakID)
		return
	} else if err != nil {
		log.WithError(err).Error("Unable to fetch vehicle.")
		return
	}

	// determine if this is a new update from itrak by comparing timestamps
	newTime, err := itrakTimeDate(result["time"], result["date"])
	if err != nil {
		log.WithError(err).Error("unable to parse iTRAK time and date")
		return
	}
	lastUpdate, err := u.ms.LatestLocation(vehicle.ID)
	if err != nil && err != shuttletracker.ErrLocationNotFound {
		log.WithError(err).Error("unable to retrieve last update")
		return
	}
	if err != shuttletracker.ErrLocationNotFound && newTime.Equal(lastUpdate.Time) {
		// Timestamp is not new; don't store update.
		return
	}
	log.Debugf("Updating %s.", vehicle.Name)

	// vehicle found and no error
	route, err := u.GuessRouteForVehicle(vehicle)
	if err != nil {
		log.WithError(err).Error("Unable to guess route for vehicle.")
		return
	}

	latitude, err := strconv.ParseFloat(strings.Replace(result["lat"], "lat:", "", -1), 64)
	if err != nil {
		log.WithError(err).Error("unable to parse latitude as float")
		return
	}
	longitude, err := strconv.ParseFloat(strings.Replace(result["lng"], "lon:", "", -1), 64)
	if err != nil {
		log.WithError(err).Error("unable to parse longitude as float")
		return
	}
	heading, err := strconv.ParseFloat(strings.Replace(result["heading"], "dir:", "", -1), 64)
	if err != nil {
		log.WithError(err).Error("unable to parse heading as float")
		return
	}
	// convert KPH to MPH
	speedKMH, err := strconv.ParseFloat(strings.Replace(result["speed"], "spd:", "", -1), 64)
	if err != nil {
		log.Error(err)
		return
	}
	speedMPH := kphToMPH(speedKMH)

	trackerID := strings.Replace(result["id"], "Vehicle ID:", "", -1)

	update := &shuttletracker.Location{
		TrackerID: trackerID,
		Latitude:  latitude,
		Longitude: longitude,
		Heading:   heading,
		Speed:     speedMPH,
		Time:      newTime,
	}

	// Only add this vehicle ID to the active vehicle list if it has a route and isn't already there
	index := -1
	for i, id := range u.vehicleIDs {
		if id == vehicle.ID {
			index = i
			break
		}
	}

	if route != nil {
		update.RouteID = &route.ID
		u.locations[vehicle.ID] = *update
		if index < 0 {
			u.vehicleIDs = append(u.vehicleIDs, vehicle.ID)
		}
	} else if index >= 0 {
		// This vehicle is no longer on a route; remove it from the active vehicles list
		u.vehicleIDs[index] = u.vehicleIDs[len(u.vehicleIDs)-1]
		u.vehicleIDs[len(u.vehicleIDs)-1] = 0
		u.vehicleIDs = u.vehicleIDs[:len(u.vehicleIDs)-1]
	}

	if err := u.ms.CreateLocation(update); err != nil {
		log.WithError(err).Errorf("could not create location")
		return
	}

	// Debug; find the route point closest to this newly updated vehicle
	index = 0
	if route != nil {
		index = smooth.ClosestPointTo(latitude, longitude, route)
	}

	// DEBUG
	if prediction, exists := u.predictions[vehicle.ID]; exists {
		diffIndex := int64(math.Abs(float64(prediction.Index - index)))
		diffDistance := smooth.DistanceBetween(prediction.Point, shuttletracker.Point{Latitude: latitude, Longitude: longitude})
		log.Debugf("UPDATED VEHICLE %d", vehicle.ID)
		log.Debugf("Predicted: %d, (%f, %f)", prediction.Index, prediction.Point.Latitude, prediction.Point.Longitude)
		log.Debugf("Actual: %d, (%f, %f)", index, latitude, longitude)
		log.Debugf("Difference: %d points or %f meters", diffIndex, diffDistance)
	}

	u.notifySubscribers(update)
}

// Convert kmh to mph
func kphToMPH(kmh float64) float64 {
	return kmh * 0.621371192
}

// GuessRouteForVehicle returns a guess at what route the vehicle is on.
// It may return an empty route if it does not believe a vehicle is on any route.
// nolint: gocyclo
func (u *Updater) GuessRouteForVehicle(vehicle *shuttletracker.Vehicle) (route *shuttletracker.Route, err error) {
	routes, err := u.ms.Routes()
	if err != nil {
		return nil, err
	}

	routeDistances := make(map[int64]float64)
	for _, route := range routes {
		routeDistances[route.ID] = 0
	}

	updates, err := u.ms.LocationsSince(vehicle.ID, time.Now().Add(time.Minute*-15))
	if len(updates) < 5 {
		// Can't make a guess with fewer than 5 updates.
		log.Debugf("%v has too few recent updates (%d) to guess route.", vehicle.Name, len(updates))
		return
	}

	for _, update := range updates {
		for _, route := range routes {
			if !route.Enabled || !route.Active {
				routeDistances[route.ID] += math.Inf(0)
				continue
			}
			nearestDistance := math.Inf(0)
			for _, point := range route.Points {
				distance := math.Sqrt(math.Pow(update.Latitude-point.Latitude, 2) +
					math.Pow(update.Longitude-point.Longitude, 2))
				if distance < nearestDistance {
					nearestDistance = distance
				}
			}
			if nearestDistance > .003 {
				nearestDistance += 50
			}
			routeDistances[route.ID] += nearestDistance
		}
	}

	minDistance := math.Inf(0)
	var minRouteID int64
	for id := range routeDistances {
		distance := routeDistances[id] / float64(len(updates))
		if distance < minDistance {
			minDistance = distance
			minRouteID = id
			// If more than ~5% of the last 100 samples were far away from a route, say the shuttle is not on a route
			// This is extremely aggressive and requires a shuttle to be on a route for ~5 minutes before it registers as on the route
			if minDistance > 5 {
				minRouteID = 0
			}
		}
	}

	// not on a route
	if minRouteID == 0 {
		log.Debugf("%v not on route; distance from nearest: %v", vehicle.Name, minDistance)
		return nil, nil
	}

	route, err = u.ms.Route(minRouteID)
	if err != nil {
		return route, err
	}
	log.Debugf("%v on %s route.", vehicle.Name, route.Name)
	return route, err
}

func itrakTimeDate(itrakTime, itrakDate string) (time.Time, error) {
	// Add leading zeros to the time value if they're missing. time.Parse expects this.
	if len(itrakTime) < 11 {
		builder := itrakTime[:5]
		for i := len(itrakTime); i < 11; i++ {
			builder += "0"
		}
		builder += itrakTime[5:]
		itrakTime = builder
	}

	combined := itrakDate + " " + itrakTime
	return time.Parse("date:01022006 time:150405", combined)
}

func (u *Updater) setLastResponse(dfresp *shuttletracker.DataFeedResponse) {
	u.mutex.Lock()
	u.lastDataFeedResponse = dfresp
	u.mutex.Unlock()
}

// GetLastResponse returns the most recent response from the iTRAK data feed.
func (u *Updater) GetLastResponse() *shuttletracker.DataFeedResponse {
	u.mutex.Lock()
	defer u.mutex.Unlock()
	return u.lastDataFeedResponse
}
