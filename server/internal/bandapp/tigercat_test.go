package bandapp

import (
	"strings"
	"testing"
)

// 橘黑猫：打开有标题和两根数值条，点猫换下一件事、震一下、存档；每个动作的帧都在安装包里
func TestTigerCat(t *testing.T) {
	a := builtinApp(t, "tiger-cat")
	files := map[string]bool{}
	for _, f := range a.Files {
		files[f.Name] = true
	}
	for act, n := range map[string]int{"sleep": 8, "work": 6, "english": 8, "tv": 8, "toy": 6, "happy": 6, "eat": 6, "hungry": 6, "sad": 6} {
		for i := 0; i < n; i++ {
			if name := "assets/" + act + "_" + string(rune('0'+i)) + ".png"; !files[name] {
				t.Fatalf("安装包里没有 %s", name)
			}
		}
	}
	if !files["icon.png"] {
		t.Fatal("安装包里没有图标")
	}

	b := runApp(t, a)
	b.waitText("点猫换个事做|饱腹")
	b.store("config", map[string]any{"sleepAt": "22", "workdays": false})
	box := findNode(b.tree(), func(n map[string]any) bool { return hasClass(n, "sprite-box") })
	b.click(box["ev"].(string))
	tree := b.waitText("陪它 15 分钟")
	if len(b.messages("vibrate")) == 0 {
		t.Fatal("点猫应该震一下")
	}
	sprite := findNode(tree, func(n map[string]any) bool { return hasClass(n, "sprite") })
	frames, _ := sprite["images"].([]any)
	if len(frames) == 0 || !strings.HasPrefix(frames[0].(map[string]any)["src"].(string), "/assets/") {
		t.Fatalf("猫的动画不对：%v", sprite["images"])
	}
	if saved := b.run("storageData['tiger-cat'] || ''"); !strings.Contains(saved, `"manual"`) {
		t.Fatalf("没存档：%s", saved)
	}
	b.waitText("今天干了什么")
}
