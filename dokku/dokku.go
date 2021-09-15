package dokku

import (
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/melbahja/goph"
)

//
type DokkuApp struct {
	Id         string
	Name       string
	Locked     bool
	ConfigVars map[string]string
	Domains    []string
	Buildpacks []string
}

//
func (app *DokkuApp) setOnResourceData(d *schema.ResourceData) {
	d.SetId(app.Id)
	d.Set("name", app.Name)
	d.Set("locked", app.Locked)

	d.Set("config_vars", app.managedConfigVars(d))

	d.Set("domains", app.Domains)
	d.Set("buildpacks", app.Buildpacks)
}

// Leave alone config vars that are set outside of terraform. This is one way
// to avoid vars that are set by dokku etc (e.g DOKKU_PROXY_PORT).
func (app *DokkuApp) managedConfigVars(d *schema.ResourceData) map[string]string {
	tfConfigKeyLookup := make(map[string]struct{})
	tfConfigVars := make(map[string]string)

	// Extract the keys that exist in d
	if c, ok := d.GetOk("config_vars"); ok {
		m := c.(map[string]interface{})
		for k := range m {
			tfConfigKeyLookup[k] = struct{}{}
		}
	}

	for varKey, varVal := range app.ConfigVars {
		if _, ok := tfConfigKeyLookup[varKey]; ok {
			tfConfigVars[varKey] = varVal
		}
	}

	return tfConfigVars
}

// TODO escape quotes
func (app *DokkuApp) configVarsStr() string {
	str := ""
	for k, v := range app.ConfigVars {
		if len(str) > 0 {
			str = str + " "
		}
		str = str + k + "=" + v
	}
	return str
}

func NewDokkuAppFromResourceData(d *schema.ResourceData) *DokkuApp {
	domains := interfaceSliceToStrSlice(d.Get("domains").(*schema.Set).List())
	buildpacks := interfaceSliceToStrSlice(d.Get("buildpacks").([]interface{}))

	configVars := make(map[string]string)
	for ck, cv := range d.Get("config_vars").(map[string]interface{}) {
		configVars[ck] = cv.(string)
	}

	return &DokkuApp{
		Name:       d.Get("name").(string),
		Locked:     d.Get("locked").(bool),
		ConfigVars: configVars,
		Domains:    domains,
		Buildpacks: buildpacks,
	}
}

//
func dokkuAppRetrieve(appName string, client *goph.Client) (*DokkuApp, error) {
	res := run(client, fmt.Sprintf("apps:exists %s", appName))

	app := &DokkuApp{Id: appName, Name: appName, Locked: false}

	if res.err != nil {
		if res.status == 20 {
			// App does not exist
			app.Id = ""
			log.Printf("[DEBUG] app %s does not exist\n", appName)
			// return nil, err
			return app, nil
		} else {
			return nil, res.err
		}
	}

	app.ConfigVars = readAppConfig(appName, client)
	domains, err := readAppDomains(appName, client)
	if err != nil {
		return nil, err
	}
	app.Domains = domains

	buildpacks, err := readAppBuildpacks(appName, client)
	if err != nil {
		return nil, err
	}
	app.Buildpacks = buildpacks

	// ssl, err := readAppSsl(appName, client)
	// if err != nil {
	// 	return nil, err
	// }
	// app.Ssl = ssl

	return app, nil
}

// TODO error handling
func readAppConfig(appName string, sshClient *goph.Client) map[string]string {
	res := run(sshClient, fmt.Sprintf("config:show %s", appName))

	// if err {
	// 	// TODO
	// }

	configLines := strings.Split(res.stdout, "\n")

	// TODO validate first line of output

	keyPairs := configLines[1:]

	config := make(map[string]string)

	for _, kp := range keyPairs {
		kp = strings.TrimSpace(kp)
		if len(kp) > 0 {
			parts := strings.Split(kp, ":")
			configKey := strings.TrimSpace(parts[0])

			configVal := parts[1]
			if len(parts[1]) > 1 {
				configVal = strings.Join(parts[1:], ":")
			}
			configVal = strings.TrimSpace(configVal)

			config[configKey] = configVal
		}
	}

	return config
}

//
func readAppDomains(appName string, client *goph.Client) ([]string, error) {
	res := run(client, fmt.Sprintf("domains:report %s", appName))

	if res.err != nil {
		return nil, res.err
	}

	domainLines := strings.Split(res.stdout, "\n")[1:]

	for _, line := range domainLines {
		parts := strings.Split(line, ":")

		key := strings.TrimSpace(parts[0])

		if key == "Domains app vhosts" {
			domainList := strings.TrimSpace(parts[1])
			if domainList == "" {
				return []string{}, nil
			} else {
				return strings.Split(domainList, " "), nil
			}
		}
	}

	// TODO proper error handling
	return nil, nil
}

// TODO Some parsing logic here that is replicated elsewhere (e.g readAppDomains above)
// which we can make reusable
func readAppBuildpacks(appName string, client *goph.Client) ([]string, error) {
	res := run(client, fmt.Sprintf("buildpacks:list %s", appName))

	if res.err != nil {
		return nil, res.err
	}

	buildpackLines := strings.Split(res.stdout, "\n")[1:]

	buildpacks := []string{}

	for _, line := range buildpackLines {
		line = strings.TrimSpace(line)
		if len(line) > 0 {
			buildpacks = append(buildpacks, line)
		}
	}

	return buildpacks, nil
}

//
func dokkuAppCreate(app *DokkuApp, client *goph.Client) error {
	res := run(client, fmt.Sprintf("apps:create %s", app.Name))

	log.Printf("[DEBUG] apps:create %v\n", res.stdout)

	if res.err != nil {
		return res.err
	}

	err := dokkuAppConfigVarsSet(app, client)

	if err != nil {
		return err
	}

	err = dokkuAppDomainsAdd(app, client)

	if err != nil {
		return err
	}

	err = dokkuAppBuildpackAdd(app.Name, app.Buildpacks, client)

	if err != nil {
		return err
	}

	return nil
}

//
func dokkuAppConfigVarsSet(app *DokkuApp, client *goph.Client) error {
	configVarStr := app.configVarsStr()
	if len(configVarStr) == 0 {
		return nil
	}

	res := run(client, fmt.Sprintf("config:set %s %s", app.Name, configVarStr))
	return res.err
}

//
func dokkuAppConfigVarsUnset(app *DokkuApp, varsToUnset []string, client *goph.Client) error {
	if len(varsToUnset) == 0 {
		return nil
	}
	log.Printf("[DEBUG] Unsetting keys %v\n", varsToUnset)
	cmd := fmt.Sprintf("config:unset %s %s", app.Name, strings.Join(varsToUnset, " "))
	log.Printf("[DEBUG] running %s", cmd)
	res := run(client, cmd)

	return res.err
}

//
func dokkuAppDomainsAdd(app *DokkuApp, client *goph.Client) error {
	domainStr := strings.Join(app.Domains, " ")

	if len(domainStr) > 0 {
		res := run(client, fmt.Sprintf("domains:set %s %s", app.Name, domainStr))
		return res.err
	}
	return nil
}

// Add buildpacks to an app based on the DokkuApp instance
func dokkuAppBuildpackAdd(appName string, buildpacks []string, client *goph.Client) error {
	for _, pack := range buildpacks {
		pack = strings.TrimSpace(pack)
		if len(pack) > 0 {
			res := run(client, fmt.Sprintf("buildpacks:add %s %s", appName, pack))

			if res.err != nil {
				return res.err
			}
		}
	}
	return nil
}

//
func dokkuAppUpdate(app *DokkuApp, d *schema.ResourceData, client *goph.Client) error {
	if d.HasChange("name") {
		old, _ := d.GetChange("name")
		res := run(client, fmt.Sprintf("apps:rename %s %s", old.(string), d.Get("name")))
		log.Printf("[DEBUG] apps:rename %s %s : %v\n", old.(string), d.Get("name"), res.stdout)
		if res.err != nil {
			return res.err
		}
	}

	appName := d.Get("name").(string)

	if d.HasChange("config_vars") {
		log.Println("[DEBUG] Changing config keys...")

		oldConfigVarsI, newConfigVarsI := d.GetChange("config_vars")
		oldConfigVars := mapOfInterfacesToMapOfStrings(oldConfigVarsI.(map[string]interface{}))
		newConfigVar := mapOfInterfacesToMapOfStrings(newConfigVarsI.(map[string]interface{}))

		keysToDelete := calculateMissingKeys(newConfigVar, oldConfigVars)

		dokkuAppConfigVarsUnset(app, keysToDelete, client)

		// TODO shouldn't need to duplicate below we already have config set function
		// This is basically an upsert, and will update values even if they haven't changed
		keysToUpsert := make([]string, len(newConfigVar))
		upsertParts := make([]string, len(newConfigVar))
		for newK, newV := range newConfigVar {
			keysToUpsert = append(keysToUpsert, newK)
			upsertParts = append(upsertParts, fmt.Sprintf("%s=%s", newK, newV))
		}

		if len(upsertParts) > 0 {
			log.Printf("[DEBUG] Setting keys %v\n", keysToUpsert)
			res := run(client, fmt.Sprintf("config:set %s %s", appName, strings.Join(upsertParts, " ")))

			if res.err != nil {
				return res.err
			}
		}
	}

	if d.HasChange("domains") {
		oldDomainsI, newDomainsI := d.GetChange("domains")
		oldDomains := interfaceSliceToStrSlice(oldDomainsI.(*schema.Set).List())
		newDomains := interfaceSliceToStrSlice(newDomainsI.(*schema.Set).List())
		domainsToRemove := calculateMissingStrings(newDomains, oldDomains)

		// Remove domains
		oldDomainsStr := strings.Join(domainsToRemove, " ")

		if len(oldDomainsStr) > 0 {
			res := run(client, fmt.Sprintf("domains:remove %s %s", appName, oldDomainsStr))

			if res.err != nil {
				return res.err
			}
		}

		// Add domains
		newDomainsStr := strings.Join(newDomains, " ")

		if len(newDomainsStr) > 0 {
			res := run(client, fmt.Sprintf("domains:add %s %s", appName, newDomainsStr))

			if res.err != nil {
				return res.err
			}
		}
	}

	if d.HasChange("buildpacks") {
		_, newBuildpacksI := d.GetChange("buildpacks")
		newBuildpacks := interfaceSliceToStrSlice(newBuildpacksI.([]interface{}))

		res := run(client, fmt.Sprintf("buildpacks:clear %s", appName))

		if res.err != nil {
			return res.err
		}
		app.Buildpacks = nil

		dokkuAppBuildpackAdd(appName, newBuildpacks, client)
	}

	return nil
}
