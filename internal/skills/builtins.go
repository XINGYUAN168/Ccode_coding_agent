package skills

// LoadBuiltins 返回编译进二进制文件的内嵌 Skill。
// 当前为空——所有 Skill 都在运行时从磁盘加载
// （用户级 ~/.ccode/skills/ 或项目级 .ccode/skills/）。
func LoadBuiltins() []*Skill {
	return nil
}
