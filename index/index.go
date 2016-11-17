package pgis

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "github.com/lib/pq"
	"github.com/whosonfirst/go-whosonfirst-crawl"
	"github.com/whosonfirst/go-whosonfirst-csv"
	"github.com/whosonfirst/go-whosonfirst-geojson"
	"github.com/whosonfirst/go-whosonfirst-placetypes"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	_ "time"
)

type Meta struct {
	Name      string           `json:"wof:name"`
	Country   string           `json:"wof:country"`
	Hierarchy []map[string]int `json:"wof:hierarchy"`
}

type Coords []float64

type Polygon []Coords

type Geometry struct {
	Type        string `json:"type"`
	Coordinates Coords `json:"coordinates"`
}

type GeometryPoly struct {
	Type        string    `json:"type"`
	Coordinates []Polygon `json:"coordinates"`
}

type PgisClient struct {
	Geometry   string
	Placetypes *placetypes.WOFPlacetypes
	Debug      bool
	Verbose    bool
	dsn        string
	conns	   chan bool
}

func NewPgisClient(host string, port int, user string, password string, dbname string, maxconns int) (*PgisClient, error) {

	pt, err := placetypes.Init()

	if err != nil {
		return nil, err
	}

	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable", host, port, user, password, dbname)

	db, err := sql.Open("postgres", dsn)

	if err != nil {
		return nil, err
	}

	defer db.Close()

	err = db.Ping()

	if err != nil {
		return nil, err
	}

	conns := make(chan bool, maxconns)

	for i := 0; i < maxconns; i++ {
	    conns <- true
	}

	client := PgisClient{
		Placetypes: pt,
		Geometry:   "", // use the default geojson geometry
		Debug:      false,
		dsn:        dsn,
		conns:	    conns,
	}

	return &client, nil
}

func (client *PgisClient) dbconn() (*sql.DB, error) {

        <- client.conns

	db, err := sql.Open("postgres", client.dsn)

	if err != nil {
		return nil, err
	}

	return db, nil
}

func (client *PgisClient) IndexFile(abs_path string, collection string) error {

	// check to see if this is an alt file
	// https://github.com/whosonfirst/go-whosonfirst-tile38/issues/1

	feature, err := geojson.UnmarshalFile(abs_path)

	if err != nil {
		return err
	}

	return client.IndexFeature(feature, collection)
}

func (client *PgisClient) IndexFeature(feature *geojson.WOFFeature, collection string) error {

	wofid := feature.Id()

	if wofid == 0 {
		log.Println("skipping Earth because it confused PostGIS")
		return nil
	}

	str_wofid := strconv.Itoa(wofid)

	body := feature.Body()

	var str_geom string

	if client.Geometry == "" {

		geom := body.Path("geometry")
		str_geom = geom.String()

	} else if client.Geometry == "bbox" {

		/*

			This is not really the best way to deal with the problem since
			we'll end up with an oversized bounding box. A better way would
			be to store the bounding box for each polygon in the geom and
			flag that in the key name. Which is easy but just requires tweaking
			a few things and really I just want to see if this works at all
			from a storage perspective right now (20160902/thisisaaronland)

		*/

		var swlon float64
		var swlat float64
		var nelon float64
		var nelat float64

		children, _ := body.S("bbox").Children()

		swlon = children[0].Data().(float64)
		swlat = children[1].Data().(float64)
		nelon = children[2].Data().(float64)
		nelat = children[3].Data().(float64)

		poly := Polygon{
			Coords{swlon, swlat},
			Coords{swlon, nelat},
			Coords{nelon, nelat},
			Coords{nelon, swlat},
			Coords{swlon, swlat},
		}

		polys := []Polygon{
			poly,
		}

		geom := GeometryPoly{
			Type:        "Polygon",
			Coordinates: polys,
		}

		bytes, err := json.Marshal(geom)

		if err != nil {
			return err
		}

		str_geom = string(bytes)

	} else if client.Geometry == "centroid" {

		// sudo put me in go-whosonfirst-geojson?
		// (20160829/thisisaaronland)

		var lat float64
		var lon float64
		var lat_ok bool
		var lon_ok bool

		lat, lat_ok = body.Path("properties.lbl:latitude").Data().(float64)
		lon, lon_ok = body.Path("properties.lbl:longitude").Data().(float64)

		if !lat_ok || !lon_ok {

			lat, lat_ok = body.Path("properties.geom:latitude").Data().(float64)
			lon, lon_ok = body.Path("properties.geom:longitude").Data().(float64)
		}

		if !lat_ok || !lon_ok {
			return errors.New("can't find centroid")
		}

		coords := Coords{lon, lat}

		geom := Geometry{
			Type:        "Point",
			Coordinates: coords,
		}

		bytes, err := json.Marshal(geom)

		if err != nil {
			return err
		}

		str_geom = string(bytes)

	} else {

		return errors.New("unknown geometry filter")
	}

	placetype := feature.Placetype()

	pt, err := client.Placetypes.GetPlacetypeByName(placetype)

	if err != nil {
		return err
	}

	repo, ok := feature.StringProperty("wof:repo")

	if !ok {
		msg := fmt.Sprintf("can't find wof:repo for %s", str_wofid)
		return errors.New(msg)
	}

	if repo == "" {
		msg := fmt.Sprintf("missing wof:repo for %s", str_wofid)
		return errors.New(msg)
	}

	key := str_wofid + "#" + repo

	parent, ok := feature.IntProperty("wof:parent_id")

	if !ok {
		log.Printf("FAILED to determine parent ID for %s\n", key)
		parent = -1
	}

	is_superseded := 0
	is_deprecated := 0

	if feature.Deprecated() {
		is_deprecated = 1
	}

	if feature.Superseded() {
		is_superseded = 1
	}

	meta_key := str_wofid + "#meta"

	name := feature.Name()
	country, ok := feature.StringProperty("wof:country")

	if !ok {
		log.Printf("FAILED to determine country for %s\n", meta_key)
		country = "XX"
	}

	hier := feature.Hierarchy()

	meta := Meta{
		Name:      name,
		Country:   country,
		Hierarchy: hier,
	}

	meta_json, err := json.Marshal(meta)

	if err != nil {
		log.Printf("FAILED to marshal JSON on %s because, %v\n", meta_key, err)
		return err
	}

	str_meta := string(meta_json)

	/*

		CREATE TABLE whosonfirst (
		id BIGINT PRIMARY KEY,
		parent_id BIGINT,
		placetype_id BIGINT,
		is_superseded SMALLINT,
		is_deprecated SMALLINT,
		meta JSON,
		geom GEOGRAPHY(MULTIPOLYGON, 4326)
		)

	*/

	// http://postgis.net/docs/ST_GeomFromGeoJSON.html
	st_geojson := fmt.Sprintf("ST_GeomFromGeoJSON('%s')", str_geom)

	if client.Verbose {

		if client.Geometry == "" {
			st_geojson = "ST_GeomFromGeoJSON('...')"
		}

		log.Println("INSERT INTO whosonfirst (id, parent_id, placetype_id, is_superseded, is_deprecated, meta, geom) VALUES (%s, %s, %s, %s, %s, %s, %s)", wofid, parent, pt.Id, is_superseded, is_deprecated, meta, st_geojson)

	}

	if !client.Debug {
		db, err := client.dbconn()

		if err != nil {
			return err
		}

		defer func() {
		      db.Close()
		      client.conns <- true
		}()

		// https://www.postgresql.org/docs/9.6/static/sql-insert.html#SQL-ON-CONFLICT
		// https://wiki.postgresql.org/wiki/What's_new_in_PostgreSQL_9.5#INSERT_..._ON_CONFLICT_DO_NOTHING.2FUPDATE_.28.22UPSERT.22.29

		sql := fmt.Sprintf("INSERT INTO whosonfirst (id, parent_id, placetype_id, is_superseded, is_deprecated, meta, geom) VALUES ($1, $2, $3, $4, $5, $6, %s) ON CONFLICT(id) DO UPDATE SET parent_id=$7, placetype_id=$8, is_superseded=$9, is_deprecated=$10, meta=$11, geom=%s", st_geojson, st_geojson)

		_, err = db.Exec(sql, wofid, parent, pt.Id, is_superseded, is_deprecated, str_meta, parent, pt.Id, is_superseded, is_deprecated, str_meta)

		if err != nil {
			return err
		}
	}

	return nil

}

func (client *PgisClient) IndexMetaFile(csv_path string, collection string, data_root string) error {

	reader, err := csv.NewDictReaderFromPath(csv_path)

	if err != nil {
		return err
	}

	count := runtime.GOMAXPROCS(0) // perversely this is how we get the count...
	ch := make(chan bool, count)

	go func() {
		for i := 0; i < count; i++ {
			ch <- true
		}
	}()

	wg := new(sync.WaitGroup)

	for {
		row, err := reader.Read()

		if err == io.EOF {
			break
		}

		if err != nil {
			return err
		}

		rel_path, ok := row["path"]

		if !ok {
			msg := fmt.Sprintf("missing 'path' column in meta file")
			return errors.New(msg)
		}

		abs_path := filepath.Join(data_root, rel_path)

		<-ch

		wg.Add(1)

		go func(ch chan bool) {

			defer func() {
				wg.Done()
				ch <- true
			}()

			client.IndexFile(abs_path, collection)

		}(ch)
	}

	wg.Wait()

	return nil
}

func (client *PgisClient) IndexDirectory(abs_path string, collection string, nfs_kludge bool) error {

	re_wof, _ := regexp.Compile(`(\d+)\.geojson$`)

	cb := func(abs_path string, info os.FileInfo) error {

		// please make me more like this...
		// https://github.com/whosonfirst/py-mapzen-whosonfirst-utils/blob/master/mapzen/whosonfirst/utils/__init__.py#L265

		fname := filepath.Base(abs_path)

		if !re_wof.MatchString(fname) {
			// log.Println("skip", abs_path)
			return nil
		}

		err := client.IndexFile(abs_path, collection)

		if err != nil {
			msg := fmt.Sprintf("failed to index %s, because %v", abs_path, err)
			return errors.New(msg)
		}

		return nil
	}

	c := crawl.NewCrawler(abs_path)
	c.NFSKludge = nfs_kludge

	return c.Crawl(cb)
}

func (client *PgisClient) IndexFileList(abs_path string, collection string) error {

	file, err := os.Open(abs_path)

	if err != nil {
		return err
	}

	defer file.Close()

	scanner := bufio.NewScanner(file)

	count := runtime.GOMAXPROCS(0) // perversely this is how we get the count...
	ch := make(chan bool, count)

	go func() {
		for i := 0; i < count; i++ {
			ch <- true
		}
	}()

	wg := new(sync.WaitGroup)

	for scanner.Scan() {

		<-ch

		path := scanner.Text()

		wg.Add(1)

		go func(path string, collection string, wg *sync.WaitGroup, ch chan bool) {

			defer wg.Done()

			client.IndexFile(path, collection)
			ch <- true

		}(path, collection, wg, ch)
	}

	wg.Wait()

	return nil
}
