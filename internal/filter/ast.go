package filter

type Node interface{ filterNode() }

type Literal struct {
	Kind LiteralKind
	Text string
}

type LiteralKind int

const (
	LiteralString LiteralKind = iota
	LiteralNumber
	LiteralBlob
	LiteralNull
	LiteralTrue
	LiteralFalse
)

type Param struct {
	Index int
}

type Column struct {
	Name string
}

type Call struct {
	Name string
	Args []Node
}

type Binary struct {
	Op    string
	Left  Node
	Right Node
}

type Unary struct {
	Op      string
	Operand Node
}

type Paren struct {
	Inner Node
}

type Is struct {
	Negated bool
	Left    Node
	Right   Node
}

type In struct {
	Negated bool
	Left    Node
	List    []Node
}

type Like struct {
	Negated bool
	Left    Node
	Pattern Node
	Escape  Node
}

type Between struct {
	Negated bool
	Value   Node
	Low     Node
	High    Node
}

type CaseBranch struct {
	When Node
	Then Node
}

type Case struct {
	Operand  Node
	Branches []CaseBranch
	Else     Node
}

func (*Literal) filterNode() {}
func (*Param) filterNode()   {}
func (*Column) filterNode()  {}
func (*Call) filterNode()    {}
func (*Binary) filterNode()  {}
func (*Unary) filterNode()   {}
func (*Paren) filterNode()   {}
func (*Is) filterNode()      {}
func (*In) filterNode()      {}
func (*Like) filterNode()    {}
func (*Between) filterNode() {}
func (*Case) filterNode()    {}

func (p *parser) push(n Node) { p.stack = append(p.stack, n) }

func (p *parser) take(n int) []Node {
	if n <= 0 || len(p.stack) < n {
		return nil
	}
	out := make([]Node, n)
	copy(out, p.stack[len(p.stack)-n:])
	p.stack = p.stack[:len(p.stack)-n]
	return out
}

func (p *parser) takeFrom(mark int) []Node {
	if mark < 0 || mark > len(p.stack) {
		return nil
	}
	out := make([]Node, len(p.stack)-mark)
	copy(out, p.stack[mark:])
	p.stack = p.stack[:mark]
	return out
}

func (p *parser) pop() Node {
	got := p.take(1)
	if len(got) != 1 {
		return nil
	}
	return got[0]
}
