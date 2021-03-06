package crons

import (
	"RPGit/app/model"
	"RPGit/app/services"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/revel/revel"
)

type ImportedData struct {
	Created    string          `json:"created_at"`
	Type       string          `json:"type"`
	User       ActorAttributes `json:"actor_attributes"`
	Repository Repository      `json:"repository"`
	Payload    Payload         `json:"payload"`
}

type ActorAttributes struct {
	Login      string `json:"login"`
	Type       string `json:"type"`
	GravatarId string `json:"gravatar_id"`
	Name       string `json:"name"`
}

type Payload struct {
	Action string `json:"action"`
}

type Repository struct {
	Language     string `json:"language"`
	Organization string `json:"organization"`
	Stars        int    `json:"stargazers_count"`
	Size         int    `json:"size"`
	Id           int    `json:"id"`
	Url          string `json:"url"`
	Description  string `json:"description"`
	Owner        string `json:"owner"`
	Name         string `json:"name"`
	Wiki         bool   `json:"has_wiki"`
	Downloads    bool   `json:"has_downloads"`
	Forks        int    `json:"forks_count"`
	Issues       int    `json:"open_issues_count"`
	IsFork       bool   `json:"fork"`
}

var steps [12]int = [12]int{5, 10, 30, 50, 100, 300, 500, 1000, 3000, 5000, 10000, 100000000}

// Import Structure that implements the Job interface
type Import struct{}

// FullImport to get all data from the beginning
type FullImport struct{}

// doImport does the import process
func doImport(date string) {
	services.ClearEventDay()
	var w sync.WaitGroup

	for i := 0; i < 24; i = i + 3 {
		w.Add(3)
		for y := 0; y < 3; y++ {
			go func(date string, i int) {
				fullPath, err := Download(fmt.Sprintf("%s-%d", date, i))
				if err != nil {
					revel.ERROR.Println(err.Error())
					return
				}
				revel.INFO.Printf("%s", fullPath)
				data, err := Ungzip(fullPath)
				if err != nil {
					revel.ERROR.Println(err.Error())
				}

				Parse(data, true)

				// Removes the file
				err = os.Remove(fullPath)
				if err != nil {
					revel.ERROR.Println(err.Error())
				}
				revel.WARN.Print(fmt.Sprintf("%s-%d", date, i))

				w.Done()
			}(date, i+y)
		}
		w.Wait()
	}

	// Updates caches
	services.ClearRankingCaches()
	revel.INFO.Print("Cache cleared")
}

// Run is the method called by the cronjob
// It downloads the archive file and update the database from the first day of the year
func (this FullImport) Run() {
	if services.IsFilled() == false {
		revel.WARN.Print("First import...")
		date := time.Now().Add(-time.Duration(48) * time.Hour).Format("2006-01-02")
		doImport(date)
	}
}

// Run is the method called by the cronjob
// It downloads the archive file and update the database
func (this Import) Run() {
	date := time.Now().Add(-time.Duration(24) * time.Hour).Format("2006-01-02")
	doImport(date)
}

// Download Downloads the archive file from githubarchive
func Download(date string) (string, error) {
	url := fmt.Sprintf("%s/%s.json.gz", revel.Config.StringDefault("imports.url", "http://data.githubarchive.org"), date)

	tokens := strings.Split(url, "/")
	file := tokens[len(tokens)-1]
	folder := revel.Config.StringDefault("imports.folder", "imports")
	fullPath := folder + "/" + file

	// Checks if the folder exists
	if _, err := os.Stat(folder); os.IsNotExist(err) {
		if err := os.Mkdir(folder, 0660); err != nil {
			return fullPath, err
		}
	}

	// Creates a file
	revel.INFO.Printf("Creating file %s", file)
	output, err := os.Create(fullPath)
	if err != nil {
		return "", err
	}
	defer output.Close()

	// Get the archive from githubarchive
	revel.INFO.Println("Downloading the file...")
	response, err := http.Get(url)
	if err != nil {
		return fullPath, err
	}
	defer response.Body.Close()

	// Write the file into the created one
	revel.INFO.Println("Copying the file...")
	bytes, err := io.Copy(output, response.Body)
	if err != nil {
		return fullPath, err
	}

	revel.INFO.Printf("File's downloading done (%d bytes)", bytes)
	return fullPath, nil
}

// Ungzip ungzip the given gzipped file and returns its content
func Ungzip(file string) (string, error) {
	// Read the file
	fileReader, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer fileReader.Close()

	reader, err := gzip.NewReader(fileReader)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	barray, err := ioutil.ReadAll(reader)
	if err != nil {
		return "", err
	}

	str := string(barray[:])
	return str, nil
}

// Parse the given json string and updates the database with it
func Parse(data string, ranking bool) {
	array := strings.Split(data, "\n")
	var total int = len(array)

	for key, event := range array {
		revel.INFO.Printf("-> Event %d/%d", (key + 1), total)

		var jsonmap ImportedData
		_ = json.Unmarshal([]byte(event), &jsonmap)

		if jsonmap.Repository.Language == "" {
			jsonmap.Repository.Language = "Unknown"
		}

		// Only user type for moment
		if jsonmap.User.Type != "User" {
			continue
		}

		// ------------------------------------- GET USER
		user := services.GetUser(strings.ToLower(jsonmap.User.Login))
		if user == nil {
			// New user
			user = model.NewUser(jsonmap.User.Login)

			// Register the user
			services.RegisterUser(user)
		}

		user.Avatar = fmt.Sprintf("http://gravatar.com/avatar/%s", jsonmap.User.GravatarId)
		user.Name = jsonmap.User.Name

		// Checks is the user is blacklisted
		if services.IsBlacklisted(user.Id) {
			continue
		}

		// ------------------------------------- GET REPOSITORY
		// No organizations
		var ownerRepo *model.User
		if jsonmap.Repository.Organization == "" {
			ownerRepo = services.GetUser(strings.ToLower(jsonmap.Repository.Owner))
			if ownerRepo == nil {
				// New user
				ownerRepo = model.NewUser(strings.ToLower(jsonmap.Repository.Owner))

				// Register the user
				services.RegisterUser(ownerRepo)
			}

			var repository *model.Repository
			if ownerRepo.Id == user.Id {
				repository = user.GetRepository(jsonmap.Repository.Id, jsonmap.Repository.Name)
			} else {
				repository = ownerRepo.GetRepository(jsonmap.Repository.Id, jsonmap.Repository.Name)
			}

			repository.Size = jsonmap.Repository.Size
			repository.Url = jsonmap.Repository.Url
			repository.Language = jsonmap.Repository.Language
			repository.Owner = strings.ToLower(jsonmap.Repository.Owner)
			repository.Organization = strings.ToLower(jsonmap.Repository.Organization)
			repository.Wiki = jsonmap.Repository.Wiki
			repository.Downloads = jsonmap.Repository.Downloads
			repository.Forks = jsonmap.Repository.Forks
			repository.Stars = jsonmap.Repository.Stars
			repository.Issues = jsonmap.Repository.Issues
			repository.IsFork = jsonmap.Repository.IsFork
			repository.Description = jsonmap.Repository.Description
		}

		language := user.GetLanguage(jsonmap.Repository.Language)

		// --------------------------------- UPDATES
		var xp int
		switch strings.ToLower(jsonmap.Type) {
		case "pushevent":
			language.Events.Pushes += 1
			for key, value := range steps {
				if jsonmap.Repository.Stars < value {
					xp = 2 + key
					break
				}
			}

		case "createevent":
			xp = 1
			language.Events.Creates += 1

		case "deleteevent":
			xp = 1
			language.Events.Deletes += 1

		case "issuesevent":
			language.Events.Issues += 1
			for key, value := range steps {
				if jsonmap.Repository.Stars < value {
					xp = 3 + key
					break
				}
			}

		case "issuecommentevent":
			language.Events.Comments += 1
			xp = 1

		case "watchevent":
			language.Events.Stars += 1
			xp = 1

		case "forkevent":
			language.Events.Forks += 1
			xp = 1

		case "pullrequestevent":
			language.Events.Pullrequests += 1
			for key, value := range steps {
				if jsonmap.Repository.Stars < value {
					xp = 5 + key
					break
				}
			}

		case "pullrequestreviewcommentevent":
			language.Events.Comments += 1
			xp = 1
		}

		// Register a daily event
		if ranking == true {
			services.RegisterEventDay(model.NewEventDay(
				strings.ToLower(jsonmap.Type),
				language.Name,
				user.Id,
				xp,
				jsonmap.Repository.Id,
				jsonmap.Created,
			))
		}

		// Updates level & experience
		language.AddExperience(xp)
		user.AddExperience(xp)

		// Updates database data
		services.UpdateUser(user)
		if jsonmap.Repository.Organization == "" && ownerRepo.Id != user.Id {
			services.UpdateUser(ownerRepo)
		}
	}
}
