package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/SpencerCornish/msubot-appspot/server/constants"

	"cloud.google.com/go/firestore"
	"github.com/PuerkitoBio/goquery"
	log "github.com/sirupsen/logrus"
)

// MakeAtlasSectionRequest makes a request to Atlas for section data in the term, department, and course
func MakeAtlasSectionRequest(client *http.Client, term, dept, course string) (*http.Response, error) {
	body := fmt.Sprintf(constants.AtlasPostFormatString,
		term,
		dept,
		course)

	req, err := http.NewRequest("POST", constants.AtlasSectionURL, strings.NewReader(body))
	defer req.Body.Close()
	if err != nil {
		return nil, err
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// ParseSectionResponse turns the http.Response into a slice of sections
func ParseSectionResponse(response *http.Response, crnToFind string) ([]constants.Section, error) {
	sections := []constants.Section{}
	doc, err := goquery.NewDocumentFromReader(response.Body)
	if err != nil {
		return nil, err
	}
	rows := doc.Find("TR")
	for i := range rows.Nodes {
		columnsFr := rows.Eq(i).Find("TD")
		columnsSr := rows.Eq(i + 1).Find("TD")

		if columnsFr.Length()+columnsSr.Length() == 15 {
			matcher := regexp.MustCompile("[A-Za-z0-9]+")

			matches := matcher.FindAllString(columnsFr.Eq(1).Text(), -1)
			if len(matches) != 3 {
				panic("regex didn't work. Did the data model change?")
			}

			newSection := constants.Section{
				DeptAbbr:       matches[0],
				CourseNumber:   matches[1],
				SectionNumber:  matches[2],
				CourseName:     strings.TrimSpace(columnsFr.Eq(2).Text()),
				Crn:            strings.TrimSpace(columnsFr.Eq(3).Text()),
				TotalSeats:     strings.TrimSpace(columnsFr.Eq(4).Text()),
				TakenSeats:     strings.TrimSpace(columnsFr.Eq(5).Text()),
				AvailableSeats: strings.TrimSpace(columnsFr.Eq(6).Text()),
				Instructor:     strings.TrimSpace(columnsFr.Eq(7).Text()),
				DeptName:       strings.TrimSpace(columnsSr.Eq(0).Text()),
				CourseType:     strings.TrimSpace(columnsSr.Eq(1).Text()),
				Time:           strings.TrimSpace(columnsSr.Eq(2).Text()),
				Location:       strings.TrimSpace(columnsSr.Eq(3).Text()),
				Credits:        strings.TrimSpace(columnsSr.Eq(4).Text()),
			}
			// Fixes recitation credits being blank, rather than 0 like they should be
			if newSection.Credits == "" {
				newSection.Credits = "0"
			}

			// We're looking for a specific section in this context,
			// so check if this is it, return it or continue if it's not
			if crnToFind != "" {
				if newSection.Crn == crnToFind {
					sections = append(sections, newSection)
					return sections, nil
				}
				continue
			}
			sections = append(sections, newSection)
		}
	}
	doc = nil
	return sections, nil
}

////////////////////////////
// Phone Functions
////////////////////////////

// SendText sends a text message to the specified phone number
func SendText(client *http.Client, number, message string) (response *http.Response, err error) {
	authID := os.Getenv("PLIVO_AUTH_ID")
	authToken := os.Getenv("PLIVO_AUTH_TOKEN")
	if authID == "" || authToken == "" {
		log.Errorf("Environment is missing required variables PLIVO_AUTH_ID and PLIVO_AUTH_TOKEN")
		return nil, err
	}
	// TODO: Create sms callback handler
	url := fmt.Sprintf(constants.PlivoAPIEndpoint, authID)
	data := constants.PlivoRequest{Src: constants.PlivoSrcNum, Dst: number, Text: message}

	js, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequest("POST", url, bytes.NewBuffer(js))
	if err != nil {
		return nil, err
	}

	request.SetBasicAuth(authID, authToken)
	request.Header.Add("Content-Type", "application/json")
	resp, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	return resp, err
}

// FetchUserDataWithNumber check firebase to see if the user exists in our database. Returns userData map and userID
func FetchUserDataWithNumber(ctx context.Context, fbClient *firestore.Client, number string) (map[string]interface{}, string) {
	checkedNumber := fmt.Sprintf("+%v", strings.Trim(number, " "))

	docs := fbClient.Collection("users").Where("number", "==", checkedNumber).Documents(ctx)

	parsed, err := docs.GetAll()
	if err != nil {
		log.WithContext(ctx).WithError(err).Errorf("DoesUserExist Error")
		return nil, ""
	}
	if len(parsed) > 0 {
		userData := parsed[0].Data()
		uid := parsed[0].Ref.ID
		return userData, uid
	}
	return nil, ""
}

// LookupUserNumber looks up a user's phone number from their uid
func LookupUserNumber(ctx context.Context, fbClient *firestore.Client, uid string) (string, error) {
	doc, err := fbClient.Collection("users").Doc(uid).Get(ctx)
	if err != nil {
		log.WithContext(ctx).WithError(err).Errorf("Tracked user not found. This should've been cleaned up")
		return "", err
	}
	return doc.Data()["number"].(string), nil
}

// GetFirebaseClient creates and returns a new firebase client, used to interact with the database
func GetFirebaseClient(ctx context.Context) (*firestore.Client, error) {
	firebasePID := os.Getenv("FIREBASE_PROJECT")
	log.WithContext(ctx).Infof("Loaded firebase project ID.")
	if firebasePID == "" {
		log.WithContext(ctx).Errorf("Firebase Project ID is nil")
		return nil, fmt.Errorf("Firebase Project ID is nil")
	}

	fbClient, err := firestore.NewClient(ctx, firebasePID)
	if err != nil {
		log.WithContext(ctx).WithError(err).Errorf("Could not create new client for Firebase")
		return nil, fmt.Errorf("Could not create new client for Firebase")
	}
	log.WithContext(ctx).Infof("successfully opened firestore client")

	return fbClient, nil
}

// MoveTrackedSection moves old sections out of the prod area
func MoveTrackedSection(ctx context.Context, fbClient *firestore.Client, crn, uid, term string) error {

	// Look for an existing archive doc to add userdata to
	docArchiveIter := fbClient.Collection("sections_archive").Where("term", "==", term).Where("crn", "==", crn).Documents(ctx)
	archiveDocs, err := docArchiveIter.GetAll()

	if err != nil {
		log.WithContext(ctx).WithError(err).Errorf("Could not get list of archive docs for uid %v: %v", uid, err)
		return err
	}

	// Get the document that we need to move
	docToMove, err := fbClient.Collection("sections_tracked").Doc(uid).Get(ctx)
	docToMoveData := docToMove.Data()

	if err != nil {
		log.WithContext(ctx).WithError(err).Errorf("Could not get the new doc for uid %s : %v", uid, err)
		return err
	}

	//  if there is a doc, merge with it rather than making a new one
	if archiveDocs != nil || len(archiveDocs) > 0 {
		if len(archiveDocs) > 1 {
			log.WithContext(ctx).Warningf("Duplicate archiveDocs: %v", archiveDocs)
		}

		//  Get the data for the archive docs
		data := archiveDocs[0].Data()

		// get all the users
		users, ok := data["users"].([]interface{})
		if !ok {
			log.WithContext(ctx).Errorf("couldn't parse all userdata")
			return fmt.Errorf("Couldn't parse all userdata")
		}

		// get all the users
		usersToAdd, ok := docToMoveData["users"].([]interface{})
		if !ok {
			log.WithContext(ctx).WithError(err).Errorf("couldn't parse userslice")
			return fmt.Errorf("couldn't parse userslice")
		}

		//  make a mega list
		allUsers := append(users, usersToAdd...)

		// Update that userlist
		_, err := archiveDocs[0].Ref.Set(ctx, map[string]interface{}{
			"users": allUsers,
		}, firestore.MergeAll)
		if err != nil {
			log.WithContext(ctx).WithError(err).Errorf("Error appending users to archive")
			return fmt.Errorf("Error appending users to archive")
		}
	} else {

		// Add a new doc
		_, _, err := fbClient.Collection("sections_archive").Add(ctx, docToMoveData)
		if err != nil {
			log.WithContext(ctx).WithError(err).Errorf("Error creating a new archived doc")
			return err
		}

	}

	//  Finally delete the old one
	_, err = docToMove.Ref.Delete(ctx)
	if err != nil {
		log.WithContext(ctx).WithError(err).Errorf("Error deleting old document")
		return err
	}
	return nil
}
