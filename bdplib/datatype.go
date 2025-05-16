// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package bdplib

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type DataType struct {
	Name        string            `json:"name"`
	Unit        string            `json:"unit"`
	Description string            `json:"description"`
	Rtype       string            `json:"rtype"`
	Period      uint32            `json:"period"`
	MetaData    map[string]string `json:"metaData"`
}

type DataTypeList struct {
	types []DataType
}

func NewDataTypeList(list []DataType) *DataTypeList {
	if nil == list {
		list = make([]DataType, 0)
	}
	return &DataTypeList{
		types: list,
	}
}

func (dl *DataTypeList) Load(file_path string) error {
	// Open the JSON file
	file, err := os.Open(file_path)
	if err != nil {
		return fmt.Errorf("datatypelist::load cannot open %s: %s", file_path, err.Error())
	}
	defer file.Close()

	// Read the file content
	byteValue, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("datatypelist::load read open %s: %s", file_path, err.Error())
	}

	// Unmarshal JSON into struct
	err = json.Unmarshal(byteValue, &dl.types)
	if err != nil {
		return fmt.Errorf("datatypelist::load unmarshal %s: %s", file_path, err.Error())
	}
	return nil
}

func (dl *DataTypeList) All() []DataType {
	return dl.types
}

func (dl *DataTypeList) Find(name string) *DataType {
	for _, t := range dl.types {
		if t.Name == name {
			return &t
		}
	}
	return nil
}
