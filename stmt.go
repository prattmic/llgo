/*
Copyright (c) 2011, 2012 Andrew Wilkins <axwalk@gmail.com>

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies
of the Software, and to permit persons to whom the Software is furnished to do
so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package llgo

import (
	"fmt"
	"github.com/axw/gollvm/llvm"
	"github.com/axw/llgo/types"
	"go/ast"
	"go/token"
	"reflect"
)

func (c *compiler) VisitIncDecStmt(stmt *ast.IncDecStmt) {
	ptr := c.VisitExpr(stmt.X).(*LLVMValue).pointer
	value := c.builder.CreateLoad(ptr.LLVMValue(), "")
	one := llvm.ConstInt(value.Type(), 1, false)

	switch stmt.Tok {
	case token.INC:
		value = c.builder.CreateAdd(value, one, "")
	case token.DEC:
		value = c.builder.CreateSub(value, one, "")
	}

	// TODO make sure we cover all possibilities (maybe just delegate this to
	// an assignment statement handler, and do it all in one place).
	//
	// In the case of a simple variable, we simply calculate the new value and
	// update the value in the scope.
	c.builder.CreateStore(value, ptr.LLVMValue())
}

func (c *compiler) VisitBlockStmt(stmt *ast.BlockStmt) {
	c.PushScope()
	defer c.PopScope()
	for _, stmt := range stmt.List {
		c.VisitStmt(stmt)
	}
}

func (c *compiler) VisitReturnStmt(stmt *ast.ReturnStmt) {
	if stmt.Results == nil {
		c.builder.CreateRetVoid()
	} else {
		if len(stmt.Results) == 1 {
			value := c.VisitExpr(stmt.Results[0])

			// Convert value to the function's return type.
			cur_fn := c.functions[len(c.functions)-1]
			fn_type := cur_fn.Type().(*types.Func)
			result := value.Convert(c.ObjGetType(fn_type.Results[0]))

			c.builder.CreateRet(result.LLVMValue())
		} else {
			// TODO handle multi-return value functions in
			// result expressions.
			// TODO convert to function's return type.
			values := make([]llvm.Value, len(stmt.Results))
			for i, expr := range stmt.Results {
				values[i] = c.VisitExpr(expr).LLVMValue()
			}
			c.builder.CreateAggregateRet(values)
		}
	}
}

func (c *compiler) VisitAssignStmt(stmt *ast.AssignStmt) {
	values := make([]Value, len(stmt.Lhs))
	if len(stmt.Rhs) == 1 && len(stmt.Lhs) > 1 {
		value := c.VisitExpr(stmt.Rhs[0])
		ptr := value.LLVMValue()
		struct_type := value.Type().(*types.Struct)
		for i := 0; i < len(struct_type.Fields); i++ {
			t := c.ObjGetType(struct_type.Fields[i])
			value_ := c.builder.CreateExtractValue(ptr, i, "")
			values[i] = c.NewLLVMValue(value_, t)
		}
	} else {
		for i, expr := range stmt.Rhs {
			values[i] = c.VisitExpr(expr)
		}
	}
	for i, expr := range stmt.Lhs {
		value := values[i]
		switch x := expr.(type) {
		case *ast.Ident:
			if x.Name != "_" {
				obj := x.Obj
				if stmt.Tok == token.DEFINE {
					value_type := value.LLVMValue().Type()
					ptr := c.builder.CreateAlloca(value_type, x.Name)
					c.builder.CreateStore(value.LLVMValue(), ptr)
					llvm_value := c.NewLLVMValue(
						ptr, &types.Pointer{Base: value.Type()})
					obj.Data = llvm_value.makePointee()
				} else {
					ptr := (obj.Data).(*LLVMValue).pointer
					value = value.Convert(types.Deref(ptr.Type()))
					c.builder.CreateStore(value.LLVMValue(), ptr.LLVMValue())
				}
			}
		default:
			ptr := c.VisitExpr(expr).(*LLVMValue).pointer
			value = value.Convert(types.Deref(ptr.Type()))
			c.builder.CreateStore(value.LLVMValue(), ptr.LLVMValue())
		}
	}
}

func (c *compiler) VisitIfStmt(stmt *ast.IfStmt) {
	curr_block := c.builder.GetInsertBlock()
	resume_block := llvm.AddBasicBlock(curr_block.Parent(), "endif")
	resume_block.MoveAfter(curr_block)

	var if_block, else_block llvm.BasicBlock
	if stmt.Else != nil {
		else_block = llvm.InsertBasicBlock(resume_block, "else")
		if_block = llvm.InsertBasicBlock(else_block, "if")
	} else {
		if_block = llvm.InsertBasicBlock(resume_block, "if")
	}
	if stmt.Else == nil {
		else_block = resume_block
	}

	if stmt.Init != nil {
		c.PushScope()
		c.VisitStmt(stmt.Init)
		defer c.PopScope()
	}

	cond_val := c.VisitExpr(stmt.Cond)
	c.builder.CreateCondBr(cond_val.LLVMValue(), if_block, else_block)
	c.builder.SetInsertPointAtEnd(if_block)
	c.VisitBlockStmt(stmt.Body)
	if in := if_block.LastInstruction(); in.IsNil() || in.IsATerminatorInst().IsNil() {
		c.builder.CreateBr(resume_block)
	}

	if stmt.Else != nil {
		c.builder.SetInsertPointAtEnd(else_block)
		c.VisitStmt(stmt.Else)
		if in := else_block.LastInstruction(); in.IsNil() || in.IsATerminatorInst().IsNil() {
			c.builder.CreateBr(resume_block)
		}
	}

	// If there's a block after the "resume" block (i.e. a nested control
	// statement), then create a branch to it as the last instruction.
	c.builder.SetInsertPointAtEnd(resume_block)
	if last := resume_block.Parent().LastBasicBlock(); last != resume_block {
		c.builder.CreateBr(last)
		c.builder.SetInsertPointBefore(resume_block.FirstInstruction())
	}
}

func (c *compiler) VisitForStmt(stmt *ast.ForStmt) {
	curr_block := c.builder.GetInsertBlock()
	var cond_block, loop_block, done_block llvm.BasicBlock
	if stmt.Cond != nil {
		cond_block = llvm.AddBasicBlock(curr_block.Parent(), "cond")
	}
	loop_block = llvm.AddBasicBlock(curr_block.Parent(), "loop")
	done_block = llvm.AddBasicBlock(curr_block.Parent(), "done")

	// Is there an initializer? Create a new scope and visit the statement.
	if stmt.Init != nil {
		c.PushScope()
		c.VisitStmt(stmt.Init)
		defer c.PopScope()
	}

	// Start the loop.
	if stmt.Cond != nil {
		c.builder.CreateBr(cond_block)
		c.builder.SetInsertPointAtEnd(cond_block)
		cond_val := c.VisitExpr(stmt.Cond)
		c.builder.CreateCondBr(
			cond_val.LLVMValue(), loop_block, done_block)
	} else {
		c.builder.CreateBr(loop_block)
	}

	// Loop body.
	c.builder.SetInsertPointAtEnd(loop_block)
	c.VisitBlockStmt(stmt.Body)
	if stmt.Post != nil {
		c.VisitStmt(stmt.Post)
	}
	if stmt.Cond != nil {
		c.builder.CreateBr(cond_block)
	} else {
		c.builder.CreateBr(loop_block)
	}
	c.builder.SetInsertPointAtEnd(done_block)
}

func (c *compiler) VisitGoStmt(stmt *ast.GoStmt) {
	//stmt.Call *ast.CallExpr
	// TODO 
	var fn *LLVMValue
	switch x := (stmt.Call.Fun).(type) {
	case *ast.Ident:
		fn = c.Resolve(x.Obj).(*LLVMValue)
		if fn == nil {
			panic(fmt.Sprintf(
				"No function found with name '%s'", x.String()))
		}
	default:
		fn = c.VisitExpr(stmt.Call.Fun).(*LLVMValue)
	}

	// Evaluate arguments, store in a structure on the stack.
	var args_struct_type llvm.Type
	var args_mem llvm.Value
	var args_size llvm.Value
	if stmt.Call.Args != nil {
		param_types := make([]llvm.Type, 0)
		fn_type := types.Deref(fn.Type()).(*types.Func)
		for _, param := range fn_type.Params {
			typ := param.Type.(types.Type)
			param_types = append(param_types, c.types.ToLLVM(typ))
		}
		args_struct_type = llvm.StructType(param_types, false)
		args_mem = c.builder.CreateAlloca(args_struct_type, "")
		for i, expr := range stmt.Call.Args {
			value_i := c.VisitExpr(expr)
			value_i = value_i.Convert(fn_type.Params[i].Type.(types.Type))
			arg_i := c.builder.CreateGEP(args_mem, []llvm.Value{
				llvm.ConstInt(llvm.Int32Type(), 0, false),
				llvm.ConstInt(llvm.Int32Type(), uint64(i), false)}, "")
			c.builder.CreateStore(value_i.LLVMValue(), arg_i)
		}
		args_size = llvm.SizeOf(args_struct_type)
		args_size = llvm.ConstTrunc(args_size, llvm.Int32Type())
	} else {
		args_struct_type = llvm.VoidType()
		args_mem = llvm.ConstNull(llvm.PointerType(args_struct_type, 0))
		args_size = llvm.ConstInt(llvm.Int32Type(), 0, false)
	}

	// When done, return to where we were.
	defer c.builder.SetInsertPointAtEnd(c.builder.GetInsertBlock())

	// Create a function that will take a pointer to a structure of the type
	// defined above, or no parameters if there are none to pass.
	indirect_fn_type := llvm.FunctionType(
		llvm.VoidType(),
		[]llvm.Type{llvm.PointerType(args_struct_type, 0)}, false)
	indirect_fn := llvm.AddFunction(c.module.Module, "", indirect_fn_type)
	indirect_fn.SetFunctionCallConv(llvm.CCallConv)

	// Call "newgoroutine" with the indirect function and stored args.
	newgoroutine := getnewgoroutine(c.module.Module)
	ngr_param_types := newgoroutine.Type().ElementType().ParamTypes()
	fn_arg := c.builder.CreateBitCast(indirect_fn, ngr_param_types[0], "")
	args_arg := c.builder.CreateBitCast(args_mem,
		llvm.PointerType(llvm.Int8Type(), 0), "")
	c.builder.CreateCall(newgoroutine,
		[]llvm.Value{fn_arg, args_arg, args_size}, "")

	entry := llvm.AddBasicBlock(indirect_fn, "entry")
	c.builder.SetInsertPointAtEnd(entry)
	var args []llvm.Value
	if stmt.Call.Args != nil {
		args_mem = indirect_fn.Param(0)
		args = make([]llvm.Value, len(stmt.Call.Args))
		for i := range stmt.Call.Args {
			arg_i := c.builder.CreateGEP(args_mem, []llvm.Value{
				llvm.ConstInt(llvm.Int32Type(), 0, false),
				llvm.ConstInt(llvm.Int32Type(), uint64(i), false)}, "")
			args[i] = c.builder.CreateLoad(arg_i, "")
		}
	}
	c.builder.CreateCall(fn.LLVMValue(), args, "")
	c.builder.CreateRetVoid()
}

func (c *compiler) VisitSwitchStmt(stmt *ast.SwitchStmt) {
	if stmt.Init != nil {
		c.PushScope()
		defer c.PopScope()
		c.VisitStmt(stmt.Init)
	}

	var tag Value
	if stmt.Tag != nil {
		tag = c.VisitExpr(stmt.Tag)
	} else {
		True := types.Universe.Lookup("true")
		tag = c.Resolve(True)
	}
	if len(stmt.Body.List) == 0 {
		return
	}

	// Create a BasicBlock for each case clause and each associated
	// statement body. Each case clause will branch to either its
	// statement body (success) or to the next case (failure), or the
	// end block if there are no remaining cases.
	startBlock := c.builder.GetInsertBlock()
	endBlock := llvm.AddBasicBlock(startBlock.Parent(), "end")
	endBlock.MoveAfter(startBlock)
	caseBlocks := make([]llvm.BasicBlock, 0, len(stmt.Body.List))
	stmtBlocks := make([]llvm.BasicBlock, 0, len(stmt.Body.List))
	for _ = range stmt.Body.List {
		caseBlocks = append(caseBlocks, llvm.InsertBasicBlock(endBlock, ""))
	}
	for _ = range stmt.Body.List {
		stmtBlocks = append(stmtBlocks, llvm.InsertBasicBlock(endBlock, ""))
	}

	c.builder.CreateBr(caseBlocks[0])
	for i, stmt := range stmt.Body.List {
		c.builder.SetInsertPointAtEnd(caseBlocks[i])
		clause := stmt.(*ast.CaseClause)
		value := c.VisitExpr(clause.List[0])
		result := value.BinaryOp(token.EQL, tag)
		for _, expr := range clause.List[1:] {
			value = c.compileLogicalOp(token.LOR, result, expr)
		}

		stmtBlock := stmtBlocks[i]
		nextBlock := endBlock
		if i+1 < len(caseBlocks) {
			nextBlock = caseBlocks[i+1]
		}
		c.builder.CreateCondBr(result.LLVMValue(), stmtBlock, nextBlock)

		c.builder.SetInsertPointAtEnd(stmtBlock)
		branchBlock := endBlock
		for _, stmt := range clause.Body {
			if br, isbr := stmt.(*ast.BranchStmt); isbr && br.Tok == token.FALLTHROUGH {
				if i+1 < len(stmtBlocks) {
					branchBlock = stmtBlocks[i+1]
				}
			} else {
				c.VisitStmt(stmt)
			}
		}
		if in := stmtBlock.LastInstruction(); in.IsNil() || in.IsATerminatorInst().IsNil() {
			c.builder.CreateBr(branchBlock)
		}
	}

	c.builder.SetInsertPointAtEnd(endBlock)
}

func (c *compiler) VisitStmt(stmt ast.Stmt) {
	if c.logger != nil {
		c.logger.Println("Compile statement:", reflect.TypeOf(stmt),
			"@", c.fileset.Position(stmt.Pos()))
	}
	switch x := stmt.(type) {
	case *ast.ReturnStmt:
		c.VisitReturnStmt(x)
	case *ast.AssignStmt:
		c.VisitAssignStmt(x)
	case *ast.IncDecStmt:
		c.VisitIncDecStmt(x)
	case *ast.IfStmt:
		c.VisitIfStmt(x)
	case *ast.ForStmt:
		c.VisitForStmt(x)
	case *ast.ExprStmt:
		c.VisitExpr(x.X)
	case *ast.BlockStmt:
		c.VisitBlockStmt(x)
	case *ast.DeclStmt:
		c.VisitDecl(x.Decl)
	case *ast.GoStmt:
		c.VisitGoStmt(x)
	case *ast.SwitchStmt:
		c.VisitSwitchStmt(x)
	default:
		panic(fmt.Sprintf("Unhandled Stmt node: %s", reflect.TypeOf(stmt)))
	}
}

// vim: set ft=go :
