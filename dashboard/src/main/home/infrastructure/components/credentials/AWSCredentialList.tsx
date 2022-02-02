import React, { useContext, useEffect, useState } from "react";
import { Context } from "shared/Context";
import api from "shared/api";
import styled from "styled-components";
import Loading from "components/Loading";
import { Operation, OperationStatus, OperationType } from "shared/types";
import { readableDate } from "shared/string_utils";
import Placeholder from "components/Placeholder";
import AWSCredentialForm from "./AWSCredentialForm";

type Props = {
  selectCredential: (aws_integration_id: number) => void;
};

type AWSCredential = {
  created_at: string;
  id: number;
  user_id: number;
  project_id: number;
  aws_arn: string;
};

const AWSCredentialsList: React.FunctionComponent<Props> = ({
  selectCredential,
}) => {
  const { currentProject, setCurrentError } = useContext(Context);
  const [isLoading, setIsLoading] = useState(true);
  const [awsCredentials, setAWSCredentials] = useState<AWSCredential[]>(null);
  const [shouldCreateCred, setShouldCreateCred] = useState(false);
  const [hasError, setHasError] = useState(false);

  useEffect(() => {
    api
      .getAWSIntegration(
        "<token>",
        {},
        {
          project_id: currentProject.id,
        }
      )
      .then(({ data }) => {
        if (!Array.isArray(data)) {
          throw Error("Data is not an array");
        }

        setAWSCredentials(data);
        setIsLoading(false);
      })
      .catch((err) => {
        console.error(err);
        setHasError(true);
        setCurrentError(err.response?.data?.error);
        setIsLoading(false);
      });
  }, [currentProject]);

  if (hasError) {
    return <Placeholder>Error</Placeholder>;
  }

  if (isLoading) {
    return (
      <Placeholder>
        <Loading />
      </Placeholder>
    );
  }

  const renderList = () => {
    return awsCredentials.map((cred) => {
      return (
        <PreviewRow key={cred.id} onClick={() => selectCredential(cred.id)}>
          <Flex>
            <i className="material-icons">account_circle</i>
            {cred.aws_arn || "arn: n/a"}
          </Flex>
          <Right>Connected at {readableDate(cred.created_at)}</Right>
        </PreviewRow>
      );
    });
  };

  const renderContents = () => {
    if (shouldCreateCred) {
      return (
        <AWSCredentialForm
          setCreatedCredential={selectCredential}
          cancel={() => {}}
        />
      );
    }

    return (
      <>
        <Description>
          Select your credentials from the list below, or create a new
          credential:
        </Description>
        {renderList()}
        <CreateNewRow onClick={() => setShouldCreateCred(true)}>
          <Flex>
            <i className="material-icons">account_circle</i>Add New AWS
            Credential
          </Flex>
        </CreateNewRow>
      </>
    );
  };

  return <AWSCredentialWrapper>{renderContents()}</AWSCredentialWrapper>;
};

export default AWSCredentialsList;

const AWSCredentialWrapper = styled.div`
  margin-top: 20px;
`;

const PreviewRow = styled.div`
  display: flex;
  align-items: center;
  padding: 12px 15px;
  color: #ffffff55;
  background: #ffffff01;
  border: 1px solid #aaaabb;
  justify-content: space-between;
  font-size: 13px;
  border-radius: 5px;
  cursor: pointer;
  margin: 16px 0;

  :hover {
    background: #ffffff10;
  }
`;

const Description = styled.div`
  width: 100%;
  font-size: 13px;
  color: #aaaabb;
  margin: 20px 0;
  display: flex;
  align-items: center;
  font-weight: 400;
`;

const CreateNewRow = styled(PreviewRow)`
  background: none;
`;

const Flex = styled.div`
  display: flex;
  color: #ffffff;
  align-items: center;
  > i {
    color: #aaaabb;
    font-size: 20px;
    margin-right: 10px;
  }
`;

const Right = styled.div`
  text-align: right;
`;
