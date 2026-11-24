package cli

import "fmt"

func (a *app) resources() error {
	switch a.command {
	case "create":
		resource, err := a.s.CreateResource(a.args[0])
		if err != nil {
			return err
		}
		return a.ok(resource, "registered "+resource.Name)
	case "list":
		limit, offset, err := a.pageOptions()
		if err != nil {
			return err
		}
		items, more, err := a.s.ListResources(limit, offset)
		if err != nil {
			return err
		}
		if a.o.yes("json") {
			return a.json(map[string]any{"resources": items, "more": more, "next_offset": offset + len(items)})
		}
		for _, r := range items {
			if r.Holder == "" {
				fmt.Fprintf(a.out, "%s  available\n", r.Name)
			} else {
				fmt.Fprintf(a.out, "%s  held by %s\n", r.Name, r.Holder)
			}
		}
		a.printNextPage(more, offset+len(items))
		return nil
	case "release":
		claim, err := a.o.require("claim")
		if err != nil {
			return err
		}
		name := a.args[0]
		if err := a.s.ReleaseResource(name, claim); err != nil {
			return err
		}
		return a.ok(map[string]string{"resource": name, "status": "released"}, "released "+name)
	}
	return usage("unknown resources command " + a.command)
}
